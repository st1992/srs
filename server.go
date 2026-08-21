package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	mediartpsdk "github.com/livekit/media-sdk/rtp"
	"github.com/livekit/sipgo"
	"github.com/livekit/sipgo/sip"
)

// locatorOperationTimeout bounds individual Redis calls so a slow/unreachable
// Redis never blocks SIP signaling handling for long.
const locatorOperationTimeout = 500 * time.Millisecond

var (
	errCallNotFound = errors.New("call not found")
	errCallClosed   = errors.New("call already ended")
)

// recorderServer is the minimal SIPREC recording SIP server.
type recorderServer struct {
	cfg          *Config
	log          *slog.Logger
	ua           *sipgo.UserAgent
	srv          *sipgo.Server
	sessions     *sessionStore
	uploader     Uploader // audio recording (.ulaw) uploader
	metaUploader Uploader // per-call metadata JSON uploader
	listener     net.PacketConn
	mediaIP      string
	// sipContactHost / sipContactPort are stamped into the Contact header of
	// every 200 OK we send. They MUST resolve back to this pod's SIP socket
	// from the SBC's perspective, otherwise in-dialog ACK/BYE either loop
	// through the proxy or get 404'd by the receiving sipgo mux.
	sipContactHost string
	sipContactPort int

	reaperStop chan struct{}

	// locator registers this pod's address in Redis for each active call, so
	// external systems can discover which pod owns a call and reach its HTTP
	// API (e.g. to POST /v1/recording/split) directly. locatorEnabled is
	// false when redis_addr isn't configured; the recorder still runs, it
	// just isn't discoverable this way.
	locator        CallLocator
	locatorEnabled bool
	apiAdvertiseIP string
	apiPort        int
	locatorTTL     time.Duration

	leaseMu     sync.Mutex
	leases      map[string]struct{}
	renewerStop chan struct{}
	renewerDone chan struct{}
	renewerOnce sync.Once
}

// NewServer constructs the SIP server, registers handlers, and prepares the
// RTP port allocator. It does not start listening until Start is called.
func NewServer(cfg *Config, uploader Uploader, metaUploader Uploader, locator CallLocator, log *slog.Logger) (*recorderServer, error) {
	mediaIP := cfg.MediaIP
	if mediaIP == "" {
		ip, err := detectMediaIP()
		if err != nil {
			return nil, fmt.Errorf("media_ip not set and auto-detection failed: %w", err)
		}
		mediaIP = ip
		log.Info("auto-detected media IP", "media_ip", mediaIP)
	}

	ua, err := sipgo.NewUA(sipgo.WithUserAgent("siprec-recorder"))
	if err != nil {
		return nil, fmt.Errorf("failed to create SIP user agent: %w", err)
	}

	srv, err := sipgo.NewServer(ua)
	if err != nil {
		return nil, fmt.Errorf("failed to create SIP server: %w", err)
	}

	// Derive the SIP Contact host/port from the configured SIP listen address.
	// If the listen address binds to 0.0.0.0 (typical with hostNetwork=true)
	// we fall back to the auto-detected non-loopback IPv4, which on a host-
	// networked pod equals the node IP — that's the address the SBC will be
	// able to reach this recorder on for in-dialog traffic.
	sipHost, sipPort, err := splitHostPort(cfg.SIPListenAddr)
	if err != nil {
		return nil, fmt.Errorf("invalid sip_listen_addr %q: %w", cfg.SIPListenAddr, err)
	}
	if sipHost == "" || sipHost == "0.0.0.0" || sipHost == "::" {
		detected, derr := detectMediaIP()
		if derr != nil {
			return nil, fmt.Errorf("sip_listen_addr binds to wildcard and IP auto-detect failed: %w", derr)
		}
		sipHost = detected
	}

	// apiAdvertiseIP is registered in Redis (as "ip:port") so external
	// systems can reach this pod's HTTP API directly. It defaults to the
	// same address the SIP Contact header uses.
	apiAdvertiseIP := cfg.APIAdvertiseIP
	if apiAdvertiseIP == "" {
		apiAdvertiseIP = sipHost
	}
	_, apiPortStr, err := net.SplitHostPort(cfg.HTTPListenAddr)
	if err != nil {
		return nil, fmt.Errorf("invalid http_listen_addr %q: %w", cfg.HTTPListenAddr, err)
	}
	apiPort, err := strconv.Atoi(apiPortStr)
	if err != nil {
		return nil, fmt.Errorf("invalid http_listen_addr port %q: %w", apiPortStr, err)
	}

	if locator == nil {
		locator = disabledLocator{reason: "not configured"}
	}

	s := &recorderServer{
		cfg:            cfg,
		log:            log,
		ua:             ua,
		srv:            srv,
		sessions:       newSessionStore(),
		uploader:       uploader,
		metaUploader:   metaUploader,
		mediaIP:        mediaIP,
		sipContactHost: sipHost,
		sipContactPort: sipPort,
		reaperStop:     make(chan struct{}),
		locator:        locator,
		locatorEnabled: cfg.RedisAddr != "",
		apiAdvertiseIP: apiAdvertiseIP,
		apiPort:        apiPort,
		locatorTTL:     time.Duration(cfg.RedisLocatorTTLSeconds) * time.Second,
		leases:         make(map[string]struct{}),
		renewerStop:    make(chan struct{}),
		renewerDone:    make(chan struct{}),
	}

	srv.OnInvite(s.onInvite)
	srv.OnAck(s.onAck)
	srv.OnBye(s.onBye)
	srv.OnOptions(s.onOptions)
	go s.locatorRenewLoop()

	return s, nil
}

// Start binds the UDP signaling socket and begins serving SIP requests.
func (s *recorderServer) Start() error {
	udpAddr, err := net.ResolveUDPAddr("udp", s.cfg.SIPListenAddr)
	if err != nil {
		return fmt.Errorf("invalid sip_listen_addr %q: %w", s.cfg.SIPListenAddr, err)
	}
	lis, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on %q: %w", s.cfg.SIPListenAddr, err)
	}
	s.listener = lis
	s.log.Info("SIP signaling listening", "addr", s.cfg.SIPListenAddr, "media_ip", s.mediaIP)

	go func() {
		defer recoverAndLog(s.log, "ServeUDP")
		if err := s.srv.ServeUDP(lis); err != nil && !isClosedErr(err) {
			s.log.Error("SIP UDP serve error", "err", err)
		}
	}()

	if s.cfg.MaxCallDurationHours > 0 {
		go s.staleSessionReaper()
	}
	return nil
}

// staleSessionReaper periodically flags (logs only; never terminates)
// sessions that have exceeded MaxCallDurationHours without a BYE — most
// commonly caused by an SBC that crashed or dropped the in-dialog BYE,
// leaving an open recording file and a leaked session behind.
func (s *recorderServer) staleSessionReaper() {
	defer recoverAndLog(s.log, "staleSessionReaper")

	interval := time.Duration(s.cfg.StaleSessionCheckIntervalSec) * time.Second
	if interval <= 0 {
		interval = 300 * time.Second
	}
	maxAge := time.Duration(s.cfg.MaxCallDurationHours) * time.Hour

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-s.reaperStop:
			return
		case <-ticker.C:
			now := time.Now()
			for _, sess := range s.sessions.Snapshot() {
				age := now.Sub(sess.CreatedAt)
				if age > maxAge {
					s.log.Log(context.Background(), LevelCritical,
						"session exceeded max call duration with no BYE; recording left open (flagged only, not terminated)",
						"event", eventSessionStale,
						"sipCallID", sess.CallID,
						"age_hours", age.Hours(),
					)
				}
			}
		}
	}
}

// Stop closes the SIP listener, finalizes all recordings, and drains uploads.
func (s *recorderServer) Stop() {
	close(s.reaperStop)
	s.stopLocatorRenewer()

	if s.srv != nil {
		_ = s.srv.Close()
	}
	if s.listener != nil {
		_ = s.listener.Close()
	}

	shutdownTime := time.Now().UTC().Format(time.RFC3339Nano)
	for _, sess := range s.sessions.DrainAll() {
		if s.locatorEnabled {
			s.stopLocatorRenewal(sess.CallID)
			locatorCtx, locatorCancel := s.locatorContext()
			if err := s.locator.Delete(locatorCtx, sess.CallID); err != nil {
				s.log.Warn("failed to delete call locator on shutdown", "err", err, "sipCallID", sess.CallID)
			}
			locatorCancel()
		}
		s.finalizeSession(sess, shutdownTime, nil, "shutdown")
	}

	timeout := time.Duration(s.cfg.ShutdownUploadTimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	s.uploader.Shutdown(ctx)
	s.metaUploader.Shutdown(ctx)
}

// onInvite handles incoming INVITEs, accepting only SIPREC INVITEs.
func (s *recorderServer) onInvite(_ *slog.Logger, req *sip.Request, tx sip.ServerTransaction) {
	defer recoverAndLog(s.log, "onInvite")
	callID := callIDValue(req)
	log := s.log.With("sipCallID", callID, "src", req.Source())

	if !IsSiprecInvite(req) {
		log.Warn("rejecting non-SIPREC INVITE", "event", eventCallRejected, "reason", "not_siprec")
		s.respond(tx, req, sip.StatusBadRequest, "Not a SIPREC INVITE", nil)
		return
	}

	if callID == "" {
		s.respond(tx, req, sip.StatusBadRequest, "Missing Call-ID", nil)
		return
	}

	// A true SIP-level retransmission (identical branch) is already matched
	// and absorbed by sipgo's transaction layer before onInvite is ever
	// called again -- anything that reaches here for a Call-ID we already
	// have a session for is a genuinely distinct transaction (a re-INVITE),
	// not network noise, and must get a real response.
	if sess, ok := s.sessions.Get(callID); ok {
		s.onReInvite(log, req, tx, sess)
		return
	}

	log.Info("processing SIPREC INVITE")

	// Capture call metadata used for file naming and event timestamps.
	// Both are derived from the same time.Now() so the Unix-millisecond
	// component in the file name matches the ISO 8601 timestamp in the event.
	//
	// DNIS/ANI are initially seeded from the SIP From/To user parts as a
	// fallback; they are overridden below once rs-metadata is parsed so we
	// record the actual called/calling phone numbers, not the SIPREC proxy
	// identifiers (SIPREC-SRS / SIPREC-SRC).
	startTime := time.Now().UTC()
	startTimeMs := startTime.UnixMilli()
	startTimeISO := startTime.Format(time.RFC3339Nano)
	dnis := sipURIUserPart(toURI(req))
	if dnis == "" {
		dnis = toURI(req)
	}
	ani := sipURIUserPart(fromURI(req))
	if ani == "" {
		ani = fromURI(req)
	}

	// 100 Trying
	s.respond(tx, req, sip.StatusTrying, "Trying", nil)

	rawSDP, err := ExtractSDPFromSiprecBody(req)
	if err != nil {
		log.Error("failed to extract SDP", "err", err, "event", eventCallRejected, "reason", "sdp_extract_failed")
		s.respond(tx, req, sip.StatusBadRequest, "Invalid SDP", nil)
		return
	}

	session, mediaBlocks, err := ParseSiprecSDP(rawSDP)
	if err != nil {
		log.Error("failed to parse SDP", "err", err, "event", eventCallRejected, "reason", "sdp_parse_failed")
		// Raw SDP can carry call PII (phone numbers in URIs); only ever
		// logged at Debug, and only on the failure path where it's needed
		// to diagnose SBC interop issues.
		log.Debug("offending SDP body", "sdp", rawSDP)
		s.respond(tx, req, sip.StatusBadRequest, "Invalid SDP", nil)
		return
	}
	_ = session
	if len(mediaBlocks) != 2 {
		log.Warn("SIPREC SDP must have exactly 2 media sections", "count", len(mediaBlocks), "event", eventCallRejected, "reason", "wrong_media_section_count")
		s.respond(tx, req, sip.StatusBadRequest, "Expected 2 media sections", nil)
		return
	}

	// Parse rs-metadata (best effort; absence is not fatal).
	var meta *SiprecMetadata
	if rawMeta, mErr := ExtractSiprecMetadata(req); mErr == nil {
		if parsed, pErr := ParseSiprecMetadata(rawMeta); pErr == nil {
			meta = parsed
		} else {
			log.Warn("failed to parse SIPREC metadata", "err", pErr)
			log.Debug("offending rs-metadata body", "metadata", rawMeta)
		}
	}

	// Override DNIS/ANI with the actual phone numbers from rs-metadata
	// call_data when present; these are far more useful for file naming than
	// the SIPREC proxy URIs in the SIP From/To headers.
	if v := metaDNIS(meta); v != "" {
		dnis = v
	}
	if v := metaANI(meta); v != "" {
		ani = v
	}

	defaultLabels := []string{"inbound", "outbound"}
	answers := make([]string, 0, 2)
	recorders := make([]*rtpRecorder, 0, 2)

	cleanup := func() {
		for _, r := range recorders {
			r.Close()
			// Hand off to the uploader so the file is not left orphaned/active.
			s.uploader.Enqueue(r.Path())
		}
	}

	for i, mb := range mediaBlocks {
		label := ExtractSiprecMediaLabel(mb)
		if label == "" {
			label = defaultLabels[i]
		}
		pcmuPT := extractPCMUPayloadType(mb)

		conn, err := mediartpsdk.ListenUDPEvenPortRange(s.cfg.RTPPortStart, s.cfg.RTPPortEnd, netip.AddrFrom4([4]byte{0, 0, 0, 0}))
		if err != nil {
			log.Error("failed to allocate RTP port", "err", err, "event", eventPortExhausted)
			cleanup()
			s.respond(tx, req, sip.StatusServiceUnavailable, "No media port", nil)
			return
		}
		port := conn.LocalAddr().(*net.UDPAddr).Port

		rec, err := newRTPRecorder(conn, s.cfg.RecordingDir, callID, dnis, ani, startTimeMs, label, pcmuPT, s.cfg.RTPNoMediaTimeoutSec, log)
		if err != nil {
			log.Error("failed to create recorder", "err", err, "event", eventCallRejected, "reason", "recorder_setup_failed")
			_ = conn.Close()
			cleanup()
			s.respond(tx, req, sip.StatusInternalServerError, "Recording setup failed", nil)
			return
		}

		s.uploader.MarkActive(rec.Path())
		recorders = append(recorders, rec)
		answers = append(answers, BuildLegAnswerSDP(s.mediaIP, port, pcmuPT, label))
	}

	combinedSDP, err := CombineSiprecAnswerSDPs(rawSDP, answers[0], answers[1])
	if err != nil {
		log.Error("failed to combine SIPREC answer SDPs", "err", err, "event", eventCallRejected, "reason", "sdp_combine_failed")
		cleanup()
		s.respond(tx, req, sip.StatusInternalServerError, "SDP combine failed", nil)
		return
	}

	// Register this pod's address in Redis so external systems can discover
	// it and reach the recording-split API directly for this call. Skipped
	// entirely (not treated as an error) when Redis isn't configured.
	locatorRegistered := false
	cleanupLocator := func() {
		if !locatorRegistered {
			return
		}
		locatorCtx, locatorCancel := s.locatorContext()
		if err := s.locator.Delete(locatorCtx, callID); err != nil {
			log.Warn("failed to delete call locator during cleanup", "err", err)
		}
		locatorCancel()
		locatorRegistered = false
	}
	if s.locatorEnabled {
		locatorValue := net.JoinHostPort(s.apiAdvertiseIP, strconv.Itoa(s.apiPort))
		locatorCtx, locatorCancel := s.locatorContext()
		err = s.locator.Register(locatorCtx, callID, locatorValue, s.locatorTTL)
		locatorCancel()
		if err != nil {
			log.Error("failed to register call locator", "err", err, "event", eventLocatorRegisterFailed)
			cleanup()
			s.respond(tx, req, sip.StatusServiceUnavailable, "Call locator unavailable", nil)
			return
		}
		locatorRegistered = true
	}

	// Start receiving RTP before sending the answer so we don't miss early
	// packets. Each leg runs in its own goroutine; recover so a panic in one
	// call's recording doesn't take down every other in-flight call.
	for _, rec := range recorders {
		go func(r *rtpRecorder) {
			defer recoverAndLog(s.log, "rtp_recorder.run")
			r.run()
		}(rec)
	}

	resp := CreateSiprecResponse(req, combinedSDP, s.sipContactHost, s.sipContactPort)
	if err := tx.Respond(resp); err != nil {
		log.Error("failed to send SIPREC 200 OK", "err", err)
		cleanup()
		cleanupLocator()
		return
	}

	sess := &recSession{
		CallID:             callID,
		SourceIP:           sourceAddr(req),
		From:               fromURI(req),
		To:                 toURI(req),
		DNIS:               dnis,
		ANI:                ani,
		Headers:            collectSIPHeaders(req),
		Metadata:           meta,
		Legs:               recorders,
		StartTime:          startTimeISO,
		CreatedAt:          startTime,
		lastSegmentStartMs: startTimeMs,
	}
	sess.beginRecordingSegmentLocked(startTime, startTimeMs)
	s.sessions.Set(callID, sess)
	if locatorRegistered {
		s.startLocatorRenewal(callID)
	}

	log.Info("SIPREC recording established",
		"event", eventCallEstablished,
		"files", sess.RecordingFiles(),
		"sip_headers", sess.Headers,
		"siprec_metadata", sess.Metadata,
	)
}

// onReInvite handles a second (or subsequent) INVITE for a Call-ID we
// already have an active recording session for. Per confirmed SBC behavior,
// these always renegotiate with the SAME SDP/media (IP/port/codec) as the
// original INVITE -- this is the same underlying media session, not a
// renegotiation -- so the existing RTP sockets/legs are left completely
// untouched: we just re-answer within the same media parameters and rotate
// the recording segment (close current .ulaw files + metadata JSON, open
// new ones), exactly like the POST /v1/recording/split API does.
func (s *recorderServer) onReInvite(log *slog.Logger, req *sip.Request, tx sip.ServerTransaction, sess *recSession) {
	log.Info("processing re-INVITE for existing SIPREC session")
	s.respond(tx, req, sip.StatusTrying, "Trying", nil)

	rawSDP, err := ExtractSDPFromSiprecBody(req)
	if err != nil {
		log.Error("failed to extract SDP from re-INVITE", "err", err, "event", eventCallRejected, "reason", "sdp_extract_failed")
		s.respond(tx, req, sip.StatusBadRequest, "Invalid SDP", nil)
		return
	}

	_, mediaBlocks, err := ParseSiprecSDP(rawSDP)
	if err != nil {
		log.Error("failed to parse re-INVITE SDP", "err", err, "event", eventCallRejected, "reason", "sdp_parse_failed")
		s.respond(tx, req, sip.StatusBadRequest, "Invalid SDP", nil)
		return
	}
	if len(mediaBlocks) != 2 {
		log.Warn("re-INVITE SIPREC SDP must have exactly 2 media sections", "count", len(mediaBlocks), "event", eventCallRejected, "reason", "wrong_media_section_count")
		s.respond(tx, req, sip.StatusBadRequest, "Expected 2 media sections", nil)
		return
	}

	defaultLabels := []string{"inbound", "outbound"}
	legs, err := matchLegsToMediaBlocks(sess.Legs, mediaBlocks, defaultLabels)
	if err != nil {
		// The re-INVITE wants media parameters this session was never
		// negotiated for. Reject rather than silently reusing the wrong
		// leg's socket -- doing so would misroute or drop audio.
		log.Error("re-INVITE media does not match existing recording legs", "err", err, "event", eventCallRejected, "reason", "reinvite_leg_mismatch")
		s.respond(tx, req, sip.StatusNotAcceptableHere, "Media does not match existing session", nil)
		return
	}

	answers := make([]string, len(legs))
	for i, leg := range legs {
		port := leg.conn.LocalAddr().(*net.UDPAddr).Port
		answers[i] = BuildLegAnswerSDP(s.mediaIP, port, leg.pcmuPT, leg.label)
	}

	combinedSDP, err := CombineSiprecAnswerSDPs(rawSDP, answers[0], answers[1])
	if err != nil {
		log.Error("failed to combine re-INVITE answer SDPs", "err", err, "event", eventCallRejected, "reason", "sdp_combine_failed")
		s.respond(tx, req, sip.StatusInternalServerError, "SDP combine failed", nil)
		return
	}

	// Parse rs-metadata from this INVITE (best effort; attached to the
	// segment it closes out below, symmetric with how a BYE's rs-metadata
	// is attached to the segment it closes).
	var reinviteMeta *SiprecMetadata
	if rawMeta, mErr := ExtractSiprecMetadata(req); mErr == nil {
		if parsed, pErr := ParseSiprecMetadata(rawMeta); pErr == nil {
			reinviteMeta = parsed
		} else {
			log.Warn("failed to parse re-INVITE SIPREC metadata", "err", pErr)
		}
	}

	resp := CreateSiprecResponse(req, combinedSDP, s.sipContactHost, s.sipContactPort)
	if err := tx.Respond(resp); err != nil {
		log.Error("failed to send re-INVITE 200 OK", "err", err)
		return
	}

	// Only rotate the segment after the far end has been told the
	// re-INVITE succeeded. A failure here is non-fatal to the call:
	// rotateSegment never partially mutates state, so on failure the call
	// simply keeps recording into its current segment.
	if _, err := s.rotateSegment(sess, time.Now().UTC(), "reinvite", nil, reinviteMeta); err != nil {
		log.Error("failed to rotate recording segment for re-INVITE", "err", err, "event", eventSegmentSplitFailed)
	}
}

// onAck completes the dialog handshake; nothing else is required for recording.
func (s *recorderServer) onAck(_ *slog.Logger, req *sip.Request, _ sip.ServerTransaction) {
	defer recoverAndLog(s.log, "onAck")
	callID := callIDValue(req)
	if s.sessions.Exists(callID) {
		s.log.Debug("received ACK for SIPREC session", "sipCallID", callID)
	}
}

// onBye terminates a SIPREC session, closes recordings, and publishes call_end.
func (s *recorderServer) onBye(_ *slog.Logger, req *sip.Request, tx sip.ServerTransaction) {
	defer recoverAndLog(s.log, "onBye")
	endTimeISO := time.Now().UTC().Format(time.RFC3339Nano)
	callID := callIDValue(req)
	s.respond(tx, req, sip.StatusOK, "OK", nil)

	sess, ok := s.sessions.Delete(callID)
	if !ok {
		return
	}

	if s.locatorEnabled {
		s.stopLocatorRenewal(callID)
		locatorCtx, locatorCancel := s.locatorContext()
		if err := s.locator.Delete(locatorCtx, callID); err != nil {
			s.log.Warn("failed to delete call locator", "err", err, "sipCallID", callID)
		}
		locatorCancel()
	}

	// Parse rs-metadata from the BYE body (best effort; carries disassociate-time).
	var byeMeta *SiprecMetadata
	if rawMeta, mErr := ExtractSiprecMetadata(req); mErr == nil {
		if parsed, pErr := ParseSiprecMetadata(rawMeta); pErr == nil {
			byeMeta = parsed
		} else {
			s.log.Warn("failed to parse BYE SIPREC metadata", "err", pErr, "sipCallID", callID)
		}
	}

	s.log.Info("received BYE for SIPREC session",
		"event", eventCallEnded,
		"sipCallID", callID,
		"sip_headers", sess.Headers,
		"siprec_metadata", sess.Metadata,
		"bye_metadata", byeMeta,
	)

	// Closes out the current segment, writes/enqueues its metadata JSON, and
	// enqueues its recording files for upload.
	s.finalizeSession(sess, endTimeISO, byeMeta, "bye")
}

// onOptions answers OPTIONS pings.
func (s *recorderServer) onOptions(_ *slog.Logger, req *sip.Request, tx sip.ServerTransaction) {
	defer recoverAndLog(s.log, "onOptions")
	resp := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
	resp.AppendHeader(sip.NewHeader("Allow", "INVITE, ACK, BYE, CANCEL, OPTIONS"))
	resp.AppendHeader(sip.NewHeader("Supported", "siprec"))
	if err := tx.Respond(resp); err != nil {
		s.log.Error("failed to respond to OPTIONS", "err", err)
	}
}

// respond is a small helper to build and send a response with an optional body.
func (s *recorderServer) respond(tx sip.ServerTransaction, req *sip.Request, code sip.StatusCode, reason string, body []byte) {
	resp := sip.NewResponseFromRequest(req, code, reason, body)
	if err := tx.Respond(resp); err != nil {
		s.log.Error("failed to send SIP response", "err", err, "code", int(code))
	}
}

// =============================================================================
// Recording split
// =============================================================================

// splitResult summarizes the outcome of a SplitRecording call: the segment
// that was just closed out, and the new one that continues the recording.
type splitResult struct {
	CallID            string
	ClosedSegment     *callSegment
	NewSegmentSeq     int
	NewRecordingFiles map[string]string
}

// SplitRecording closes out the call's current recording segment (its
// .ulaw files + metadata JSON) and starts a new one, without dropping any
// in-flight RTP. metadata is attached to the new segment going forward; the
// SIPREC rs-metadata captured at INVITE time stays associated with every
// segment via callMetadataRecord.InviteMetadata.
func (s *recorderServer) SplitRecording(ctx context.Context, callID string, metadata map[string]any) (*splitResult, error) {
	sess, ok := s.sessions.Get(callID)
	if !ok {
		sess, ok = s.sessions.GetByPrefix(callID)
	}
	if !ok {
		return nil, errCallNotFound
	}
	return s.rotateSegment(sess, time.Now().UTC(), "api_split", metadata, nil)
}

// rotateSegment closes out sess's current recording segment (its .ulaw
// files + metadata JSON) and starts a new one, without dropping any
// in-flight RTP. It is the shared core behind both the HTTP split API
// (SplitRecording) and a mid-call re-INVITE on an existing Call-ID
// (onReInvite), which are otherwise identical except for what triggered the
// rotation and where the caller-supplied metadata comes from.
//
// requestMetadata is attached to the NEW segment going forward (e.g. the
// split API caller's metadata dict). closedSegmentMeta, if non-nil, is
// attached to the JSON record of the segment being CLOSED -- e.g. the
// rs-metadata carried by the re-INVITE that triggered this rotation,
// symmetric with how finalizeSession attaches byeMeta to the segment it
// closes.
func (s *recorderServer) rotateSegment(sess *recSession, now time.Time, reason string, requestMetadata map[string]any, closedSegmentMeta *SiprecMetadata) (*splitResult, error) {
	newSinks := make(map[string]*fileSink, len(sess.Legs))

	sess.mu.Lock()
	if sess.closed {
		sess.mu.Unlock()
		return nil, errCallClosed
	}

	startMs := sess.nextSegmentStartMsLocked(now)
	for _, leg := range sess.Legs {
		sink, err := newFileSink(s.cfg.RecordingDir, sess.CallID, sess.DNIS, sess.ANI, startMs, leg.label)
		if err != nil {
			sess.mu.Unlock()
			for _, created := range newSinks {
				_ = created.Close()
			}
			return nil, fmt.Errorf("create new recording sink for label %s: %w", leg.label, err)
		}
		newSinks[leg.label] = sink
	}

	var oldSinks []rtpSink
	for _, leg := range sess.Legs {
		oldSinks = append(oldSinks, leg.ReplaceSink(newSinks[leg.label]))
	}

	completed := sess.completeCurrentSegmentLocked(now, reason)
	sess.beginRecordingSegmentLocked(now, startMs)
	sess.CurrentSegment.RequestMetadata = requestMetadata
	newSeq := sess.CurrentSegment.Sequence
	newFiles := sess.CurrentSegment.RecordingFiles
	sess.mu.Unlock()

	for _, sink := range oldSinks {
		if sink == nil {
			continue
		}
		if err := sink.Close(); err != nil {
			s.log.Error("failed to close pre-split recording sink", "err", err, "sipCallID", sess.CallID, "file", sink.Path())
		}
		if p := sink.Path(); p != "" {
			s.uploader.Enqueue(p)
		}
	}
	for _, sink := range newSinks {
		s.uploader.MarkActive(sink.Path())
	}

	if completed != nil {
		if metaPath, err := s.writeSegmentMetadataJSON(sess, completed, nil, closedSegmentMeta); err != nil {
			s.log.Error("failed to write pre-split metadata JSON", "err", err, "sipCallID", sess.CallID, "event", eventSegmentSplitFailed)
		} else {
			s.metaUploader.Enqueue(metaPath)
		}
	}

	s.log.Info("split call recording into a new segment",
		"event", eventSegmentSplit,
		"sipCallID", sess.CallID,
		"reason", reason,
		"closed_segment", completed,
		"new_segment_sequence", newSeq,
		"new_recording_files", newFiles,
	)

	return &splitResult{
		CallID:            sess.CallID,
		ClosedSegment:     completed,
		NewSegmentSeq:     newSeq,
		NewRecordingFiles: newFiles,
	}, nil
}

// finalizeSession closes out the call's current (and final) segment,
// writes/enqueues its metadata JSON, and enqueues its recording files for
// upload. Used by both onBye and Stop (shutdown).
func (s *recorderServer) finalizeSession(sess *recSession, endTimeISO string, byeMeta *SiprecMetadata, reason string) {
	endTime, err := time.Parse(time.RFC3339Nano, endTimeISO)
	if err != nil {
		endTime = time.Now().UTC()
	}

	completed, legs := sess.finalize(endTime, reason)
	for _, leg := range legs {
		leg.Close()
	}

	if completed == nil {
		return
	}
	if metaPath, err := s.writeSegmentMetadataJSON(sess, completed, byeMeta, nil); err != nil {
		s.log.Error("failed to write call metadata JSON", "err", err, "sipCallID", sess.CallID)
	} else {
		s.metaUploader.Enqueue(metaPath)
		s.log.Info("enqueued call metadata JSON for upload", "file", metaPath)
	}
	for _, p := range completed.RecordingFiles {
		s.uploader.Enqueue(p)
	}
}

// =============================================================================
// Call locator (Redis)
// =============================================================================

func (s *recorderServer) locatorContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), locatorOperationTimeout)
}

func (s *recorderServer) startLocatorRenewal(callID string) {
	s.leaseMu.Lock()
	s.leases[callID] = struct{}{}
	s.leaseMu.Unlock()
}

func (s *recorderServer) stopLocatorRenewal(callID string) {
	s.leaseMu.Lock()
	delete(s.leases, callID)
	s.leaseMu.Unlock()
}

// locatorRenewLoop periodically renews the Redis TTL for every active call
// locator lease so Redis doesn't expire the entry out from under a
// still-active call.
func (s *recorderServer) locatorRenewLoop() {
	defer close(s.renewerDone)
	ttl := s.locatorTTL
	if ttl <= 0 {
		return
	}
	interval := ttl / 2
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.renewerStop:
			return
		case <-ticker.C:
			callIDs := s.activeLocatorCallIDs()
			if len(callIDs) == 0 {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), locatorOperationTimeout)
			err := s.locator.RenewMany(ctx, callIDs, ttl)
			cancel()
			if err != nil {
				s.log.Warn("failed to renew call locators", "err", err, "count", len(callIDs))
			}
		}
	}
}

func (s *recorderServer) activeLocatorCallIDs() []string {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	callIDs := make([]string, 0, len(s.leases))
	for callID := range s.leases {
		callIDs = append(callIDs, callID)
	}
	return callIDs
}

func (s *recorderServer) stopLocatorRenewer() {
	if s.renewerStop == nil {
		return
	}
	s.renewerOnce.Do(func() {
		close(s.renewerStop)
		<-s.renewerDone
	})
}

// =============================================================================
// Metadata JSON
// =============================================================================

// callMetadataRecord is written to GCS as a JSON file once per call. It
// captures everything known at both INVITE and BYE time so that downstream
// consumers have a single, self-contained document per call.
type callMetadataRecord struct {
	SIPCallID        string            `json:"sip_call_id"`
	From             string            `json:"from,omitempty"`
	To               string            `json:"to,omitempty"`
	SourceIP         string            `json:"source_ip,omitempty"`
	StartTime        string            `json:"start_time,omitempty"`
	EndTime          string            `json:"end_time,omitempty"`
	RecordingFiles   map[string]string `json:"recording_files,omitempty"`
	SIPHeaders       map[string]string `json:"sip_headers,omitempty"`
	InviteMetadata   *SiprecMetadata   `json:"invite_metadata,omitempty"`
	ByeMetadata      *SiprecMetadata   `json:"bye_metadata,omitempty"`
	ReinviteMetadata *SiprecMetadata   `json:"reinvite_metadata,omitempty"`
	Segment          *callSegment      `json:"segment,omitempty"`
}

// writeSegmentMetadataJSON serialises a single completed recording segment
// to a JSON file in the recording directory and returns its path. The
// filename shares the same stem as that segment's recording files:
// {callID}-{dnis}-{ani}-{startMs}.json, using that segment's own start-time
// timestamp (the same one embedded in its .ulaw file names, see
// newFileSink) so the JSON and its matching .ulaw files can always be
// correlated.
func (s *recorderServer) writeSegmentMetadataJSON(sess *recSession, seg *callSegment, byeMeta *SiprecMetadata, reinviteMeta *SiprecMetadata) (string, error) {
	if seg == nil {
		return "", fmt.Errorf("missing metadata segment")
	}
	record := &callMetadataRecord{
		SIPCallID:        sess.CallID,
		From:             sess.From,
		To:               sess.To,
		SourceIP:         sess.SourceIP,
		StartTime:        seg.StartTime,
		EndTime:          seg.EndTime,
		RecordingFiles:   seg.RecordingFiles,
		SIPHeaders:       sess.Headers,
		InviteMetadata:   sess.Metadata,
		ByeMetadata:      byeMeta,
		ReinviteMetadata: reinviteMeta,
		Segment:          seg,
	}

	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal metadata: %w", err)
	}

	name := fmt.Sprintf("%s-%s-%s-%d.json",
		sanitizeFileComponent(sess.CallID),
		sanitizeFileComponent(sess.DNIS),
		sanitizeFileComponent(sess.ANI),
		seg.StartMs,
	)
	p := filepath.Join(s.cfg.RecordingDir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		return "", fmt.Errorf("write metadata file: %w", err)
	}
	return p, nil
}

// =============================================================================
// Helpers
// =============================================================================

func callIDValue(req *sip.Request) string {
	if h := req.CallID(); h != nil {
		return h.Value()
	}
	return ""
}

func sourceAddr(req *sip.Request) string {
	return req.Source()
}

func fromURI(req *sip.Request) string {
	if h := req.From(); h != nil {
		return h.Address.String()
	}
	return ""
}

func toURI(req *sip.Request) string {
	if h := req.To(); h != nil {
		return h.Address.String()
	}
	return ""
}

// collectSIPHeaders extracts a selection of useful SIP headers from the INVITE.
func collectSIPHeaders(req *sip.Request) map[string]string {
	headers := make(map[string]string)
	add := func(name string) {
		if h := req.GetHeader(name); h != nil {
			headers[name] = h.Value()
		}
	}
	add("Call-ID")
	add("From")
	add("To")
	add("Contact")
	add("User-Agent")
	add("Subject")
	add("CSeq")
	return headers
}

func isClosedErr(err error) bool {
	return err != nil && (err == net.ErrClosed || err.Error() == "use of closed network connection")
}

// splitHostPort splits "host:port" into its parts, returning the port as int.
// Accepts an empty host (e.g. ":5060") and reports it as "".
func splitHostPort(addr string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, err
	}
	port, err := net.LookupPort("udp", portStr)
	if err != nil {
		return "", 0, fmt.Errorf("invalid port %q: %w", portStr, err)
	}
	return host, port, nil
}

// detectMediaIP picks the first globally-routable IPv4 address on the host.
//
// We MUST skip:
//   - loopback (127.0.0.0/8)             — never routable off-host.
//   - link-local (169.254.0.0/16)        — on GCE this is the metadata
//     server alias and is NOT routable across the VPC. If we advertise it
//     as our Contact, upstream proxies will dutifully forward in-dialog
//     ACK/BYE there and the packet will dead-end on the source host.
//   - unspecified (0.0.0.0)              — invalid as a Contact.
//   - multicast / broadcast              — invalid as a Contact.
//
// With hostNetwork: true on GKE the surviving interface is the node's
// primary VPC IP (e.g. 10.x / 100.x), which is exactly what we want
// upstream SBCs and proxies to use for in-dialog routing.
func detectMediaIP() (string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipNet.IP
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
			ip.IsUnspecified() || ip.IsMulticast() {
			continue
		}
		ip4 := ip.To4()
		if ip4 == nil {
			continue
		}
		return ip4.String(), nil
	}
	return "", fmt.Errorf("no routable non-loopback IPv4 address found")
}
