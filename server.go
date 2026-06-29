package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	mediartpsdk "github.com/livekit/media-sdk/rtp"
	"github.com/livekit/sipgo"
	"github.com/livekit/sipgo/sip"
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
}

// NewServer constructs the SIP server, registers handlers, and prepares the
// RTP port allocator. It does not start listening until Start is called.
func NewServer(cfg *Config, uploader Uploader, metaUploader Uploader, log *slog.Logger) (*recorderServer, error) {
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
	}

	srv.OnInvite(s.onInvite)
	srv.OnAck(s.onAck)
	srv.OnBye(s.onBye)
	srv.OnOptions(s.onOptions)

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
		if err := s.srv.ServeUDP(lis); err != nil && !isClosedErr(err) {
			s.log.Error("SIP UDP serve error", "err", err)
		}
	}()
	return nil
}

// Stop closes the SIP listener, finalizes all recordings, and drains uploads.
func (s *recorderServer) Stop() {
	if s.srv != nil {
		_ = s.srv.Close()
	}
	if s.listener != nil {
		_ = s.listener.Close()
	}

	shutdownTime := time.Now().UTC().Format(time.RFC3339Nano)
	for _, sess := range s.sessions.DrainAll() {
		sess.Close()
		if metaPath, err := s.writeMetadataJSON(sess, shutdownTime, nil); err != nil {
			s.log.Error("failed to write call metadata JSON on shutdown", "err", err, "sipCallID", sess.CallID)
		} else {
			s.metaUploader.Enqueue(metaPath)
		}
		for _, p := range sess.RecordingFiles() {
			s.uploader.Enqueue(p)
		}
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
	callID := callIDValue(req)
	log := s.log.With("sipCallID", callID, "src", req.Source())

	if !IsSiprecInvite(req) {
		log.Warn("rejecting non-SIPREC INVITE")
		s.respond(tx, req, sip.StatusBadRequest, "Not a SIPREC INVITE", nil)
		return
	}

	if callID == "" {
		s.respond(tx, req, sip.StatusBadRequest, "Missing Call-ID", nil)
		return
	}

	if s.sessions.Exists(callID) {
		log.Debug("ignoring SIPREC INVITE retransmission")
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
		log.Error("failed to extract SDP", "err", err)
		s.respond(tx, req, sip.StatusBadRequest, "Invalid SDP", nil)
		return
	}

	session, mediaBlocks, err := ParseSiprecSDP(rawSDP)
	if err != nil {
		log.Error("failed to parse SDP", "err", err)
		s.respond(tx, req, sip.StatusBadRequest, "Invalid SDP", nil)
		return
	}
	_ = session
	if len(mediaBlocks) != 2 {
		log.Warn("SIPREC SDP must have exactly 2 media sections", "count", len(mediaBlocks))
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
			log.Error("failed to allocate RTP port", "err", err)
			cleanup()
			s.respond(tx, req, sip.StatusServiceUnavailable, "No media port", nil)
			return
		}
		port := conn.LocalAddr().(*net.UDPAddr).Port

		rec, err := newRTPRecorder(conn, s.cfg.RecordingDir, callID, dnis, ani, startTimeMs, label, pcmuPT, log)
		if err != nil {
			log.Error("failed to create recorder", "err", err)
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
		log.Error("failed to combine SIPREC answer SDPs", "err", err)
		cleanup()
		s.respond(tx, req, sip.StatusInternalServerError, "SDP combine failed", nil)
		return
	}

	// Start receiving RTP before sending the answer so we don't miss early packets.
	for _, rec := range recorders {
		go rec.run()
	}

	resp := CreateSiprecResponse(req, combinedSDP, s.sipContactHost, s.sipContactPort)
	if err := tx.Respond(resp); err != nil {
		log.Error("failed to send SIPREC 200 OK", "err", err)
		cleanup()
		return
	}

	sess := &recSession{
		CallID:    callID,
		SourceIP:  sourceAddr(req),
		From:      fromURI(req),
		To:        toURI(req),
		DNIS:      dnis,
		ANI:       ani,
		Headers:   collectSIPHeaders(req),
		Metadata:  meta,
		Legs:      recorders,
		StartTime: startTimeISO,
		CreatedAt: startTime,
	}
	s.sessions.Set(callID, sess)

	log.Info("SIPREC recording established",
		"files", sess.RecordingFiles(),
		"sip_headers", sess.Headers,
		"siprec_metadata", sess.Metadata,
	)
}

// onAck completes the dialog handshake; nothing else is required for recording.
func (s *recorderServer) onAck(_ *slog.Logger, req *sip.Request, _ sip.ServerTransaction) {
	callID := callIDValue(req)
	if s.sessions.Exists(callID) {
		s.log.Debug("received ACK for SIPREC session", "sipCallID", callID)
	}
}

// onBye terminates a SIPREC session, closes recordings, and publishes call_end.
func (s *recorderServer) onBye(_ *slog.Logger, req *sip.Request, tx sip.ServerTransaction) {
	endTimeISO := time.Now().UTC().Format(time.RFC3339Nano)
	callID := callIDValue(req)
	s.respond(tx, req, sip.StatusOK, "OK", nil)

	sess, ok := s.sessions.Delete(callID)
	if !ok {
		return
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
		"sipCallID", callID,
		"sip_headers", sess.Headers,
		"siprec_metadata", sess.Metadata,
		"bye_metadata", byeMeta,
	)
	sess.Close()

	// Write per-call metadata JSON and enqueue it for upload to the metadata
	// bucket. Errors are non-fatal; audio recording upload still proceeds.
	if metaPath, err := s.writeMetadataJSON(sess, endTimeISO, byeMeta); err != nil {
		s.log.Error("failed to write call metadata JSON", "err", err, "sipCallID", callID)
	} else {
		s.metaUploader.Enqueue(metaPath)
		s.log.Info("enqueued call metadata JSON for upload", "file", metaPath)
	}

	// Recording files are now closed; upload them (and delete locally on success).
	for _, p := range sess.RecordingFiles() {
		s.uploader.Enqueue(p)
	}
}

// onOptions answers OPTIONS pings.
func (s *recorderServer) onOptions(_ *slog.Logger, req *sip.Request, tx sip.ServerTransaction) {
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
// Metadata JSON
// =============================================================================

// callMetadataRecord is written to GCS as a JSON file once per call. It
// captures everything known at both INVITE and BYE time so that downstream
// consumers have a single, self-contained document per call.
type callMetadataRecord struct {
	SIPCallID      string            `json:"sip_call_id"`
	From           string            `json:"from,omitempty"`
	To             string            `json:"to,omitempty"`
	SourceIP       string            `json:"source_ip,omitempty"`
	StartTime      string            `json:"start_time,omitempty"`
	EndTime        string            `json:"end_time,omitempty"`
	RecordingFiles map[string]string `json:"recording_files,omitempty"`
	SIPHeaders     map[string]string `json:"sip_headers,omitempty"`
	InviteMetadata *SiprecMetadata   `json:"invite_metadata,omitempty"`
	ByeMetadata    *SiprecMetadata   `json:"bye_metadata,omitempty"`
}

// writeMetadataJSON serialises call metadata to a JSON file in the recording
// directory and returns its path. The filename shares the same stem as the
// recording files: {callID}-{dnis}-{ani}-{startTimeMs}.json, so the JSON and
// its matching .ulaw files can always be correlated by dropping the suffix.
func (s *recorderServer) writeMetadataJSON(sess *recSession, endTimeISO string, byeMeta *SiprecMetadata) (string, error) {
	record := &callMetadataRecord{
		SIPCallID:      sess.CallID,
		From:           sess.From,
		To:             sess.To,
		SourceIP:       sess.SourceIP,
		StartTime:      sess.StartTime,
		EndTime:        endTimeISO,
		RecordingFiles: sess.RecordingFiles(),
		SIPHeaders:     sess.Headers,
		InviteMetadata: sess.Metadata,
		ByeMetadata:    byeMeta,
	}

	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal metadata: %w", err)
	}

	name := fmt.Sprintf("%s-%s-%s-%d.json",
		sanitizeFileComponent(sess.CallID),
		sanitizeFileComponent(sess.DNIS),
		sanitizeFileComponent(sess.ANI),
		sess.CreatedAt.UnixMilli(),
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
