package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"time"

	mediartpsdk "github.com/livekit/media-sdk/rtp"
	"github.com/livekit/sipgo"
	"github.com/livekit/sipgo/sip"
)

// recorderServer is the minimal SIPREC recording SIP server.
type recorderServer struct {
	cfg      *Config
	log      *slog.Logger
	ua       *sipgo.UserAgent
	srv      *sipgo.Server
	sessions *sessionStore
	pub      Publisher
	uploader Uploader
	listener net.PacketConn
	mediaIP  string
}

// NewServer constructs the SIP server, registers handlers, and prepares the
// RTP port allocator. It does not start listening until Start is called.
func NewServer(cfg *Config, pub Publisher, uploader Uploader, log *slog.Logger) (*recorderServer, error) {
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

	s := &recorderServer{
		cfg:      cfg,
		log:      log,
		ua:       ua,
		srv:      srv,
		sessions: newSessionStore(),
		pub:      pub,
		uploader: uploader,
		mediaIP:  mediaIP,
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

	for _, sess := range s.sessions.DrainAll() {
		sess.Close()
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

		rec, err := newRTPRecorder(conn, s.cfg.RecordingDir, callID, label, pcmuPT, log)
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

	resp := CreateSiprecResponse(req, combinedSDP)
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
		Headers:   collectSIPHeaders(req),
		Metadata:  meta,
		Legs:      recorders,
		CreatedAt: time.Now(),
	}
	s.sessions.Set(callID, sess)

	s.pub.Publish(context.Background(), &SiprecEvent{
		Event:          EventCallStart,
		SIPCallID:      callID,
		From:           sess.From,
		To:             sess.To,
		SourceIP:       sess.SourceIP,
		RecordingFiles: sess.RecordingFiles(),
		SiprecMetadata: sess.Metadata,
		SIPHeaders:     sess.Headers,
	})

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
	callID := callIDValue(req)
	s.respond(tx, req, sip.StatusOK, "OK", nil)

	sess, ok := s.sessions.Delete(callID)
	if !ok {
		return
	}

	s.log.Info("received BYE for SIPREC session",
		"sipCallID", callID,
		"sip_headers", sess.Headers,
		"siprec_metadata", sess.Metadata,
	)
	sess.Close()

	// Recording files are now closed; upload them (and delete locally on success).
	for _, p := range sess.RecordingFiles() {
		s.uploader.Enqueue(p)
	}

	s.pub.Publish(context.Background(), &SiprecEvent{
		Event:          EventCallEnd,
		SIPCallID:      callID,
		From:           sess.From,
		To:             sess.To,
		SourceIP:       sess.SourceIP,
		RecordingFiles: sess.RecordingFiles(),
		SiprecMetadata: sess.Metadata,
		SIPHeaders:     sess.Headers,
		Reason:         "bye",
	})
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

// detectMediaIP picks the first non-loopback IPv4 address on the host.
func detectMediaIP() (string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() {
			continue
		}
		if ip4 := ipNet.IP.To4(); ip4 != nil {
			return ip4.String(), nil
		}
	}
	return "", fmt.Errorf("no non-loopback IPv4 address found")
}
