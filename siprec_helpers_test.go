package main

import (
	"testing"

	"github.com/livekit/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// SIPREC Detection Tests
// =============================================================================

func TestIsSiprecInvite(t *testing.T) {
	tests := []struct {
		name     string
		headers  map[string]string
		expected bool
	}{
		{name: "Regular INVITE", headers: map[string]string{}, expected: false},
		{name: "Require siprec", headers: map[string]string{"Require": "siprec"}, expected: true},
		{name: "Require SIPREC uppercase", headers: map[string]string{"Require": "SIPREC"}, expected: true},
		{name: "Require multiple values", headers: map[string]string{"Require": "100rel, siprec"}, expected: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := sip.NewRequest(sip.INVITE, sip.Uri{User: "test", Host: "example.com"})
			for k, v := range tt.headers {
				req.AppendHeader(sip.NewHeader(k, v))
			}
			assert.Equal(t, tt.expected, IsSiprecInvite(req))
		})
	}
}

func TestIsSiprecInvite_NilRequest(t *testing.T) {
	assert.False(t, IsSiprecInvite(nil))
}

func TestIsSiprecInvite_NonInvite(t *testing.T) {
	req := sip.NewRequest(sip.BYE, sip.Uri{User: "test", Host: "example.com"})
	req.AppendHeader(sip.NewHeader("Require", "siprec"))
	assert.False(t, IsSiprecInvite(req))
}

// =============================================================================
// Multipart Extraction Tests
// =============================================================================

func TestExtractSDPFromMultipart(t *testing.T) {
	contentType := "multipart/mixed;boundary=\"----=_Part_15296_823292916.1768489452115\""

	sdp, err := extractSDPFromMultipart(contentType, []byte(testMultipartBody))
	require.NoError(t, err)

	assert.Contains(t, sdp, "v=0")
	assert.Contains(t, sdp, "m=audio 17588")
	assert.Contains(t, sdp, "a=label:inbound")
}

func TestExtractSDPFromSiprecBody_PlainSDP(t *testing.T) {
	req := sip.NewRequest(sip.INVITE, sip.Uri{User: "1111", Host: "example.com"})
	ct := sip.ContentTypeHeader("application/sdp")
	req.AppendHeader(&ct)
	req.SetBody([]byte(testSiprecSDP))

	sdp, err := ExtractSDPFromSiprecBody(req)
	require.NoError(t, err)
	assert.Contains(t, sdp, "m=audio 17588")
}

func TestExtractSDPFromSiprecBody_Multipart(t *testing.T) {
	req := sip.NewRequest(sip.INVITE, sip.Uri{User: "1111", Host: "example.com"})
	ct := sip.ContentTypeHeader("multipart/mixed;boundary=\"----=_Part_15296_823292916.1768489452115\"")
	req.AppendHeader(&ct)
	req.SetBody([]byte(testMultipartBody))

	sdp, err := ExtractSDPFromSiprecBody(req)
	require.NoError(t, err)
	assert.Contains(t, sdp, "m=audio 17588")
	assert.Contains(t, sdp, "a=label:outbound")
}

func TestExtractSiprecMetadata(t *testing.T) {
	req := sip.NewRequest(sip.INVITE, sip.Uri{User: "1111", Host: "example.com"})
	ct := sip.ContentTypeHeader("multipart/mixed;boundary=\"----=_Part_15296_823292916.1768489452115\"")
	req.AppendHeader(&ct)
	req.SetBody([]byte(testMultipartBody))

	meta, err := ExtractSiprecMetadata(req)
	require.NoError(t, err)
	assert.Contains(t, meta, "<recording")
	assert.Contains(t, meta, "<datamode>complete</datamode>")
}

// =============================================================================
// Response Creation Tests
// =============================================================================

func TestCreateSiprecResponse(t *testing.T) {
	req := sip.NewRequest(sip.INVITE, sip.Uri{User: "1111", Host: "example.com"})
	req.AppendHeader(&sip.FromHeader{
		DisplayName: "Recorder",
		Address:     sip.Uri{User: "SRC", Host: "sip.provider.com"},
		Params:      sip.NewParams(),
	})
	req.AppendHeader(&sip.ToHeader{
		Address: sip.Uri{User: "1111", Host: "example.com"},
		Params:  sip.NewParams(),
	})
	callID := sip.CallIDHeader("test-call-id-12345")
	req.AppendHeader(&callID)
	req.AppendHeader(&sip.CSeqHeader{SeqNo: 1, MethodName: sip.INVITE})

	combinedSDP := "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=test\r\nt=0 0\r\nm=audio 5000 RTP/AVP 0\r\na=recvonly\r\n"

	resp := CreateSiprecResponse(req, combinedSDP)

	assert.Equal(t, sip.StatusCode(200), resp.StatusCode)
	assert.Equal(t, "OK", resp.Reason)

	supported := resp.GetHeader("Supported")
	require.NotNil(t, supported)
	assert.Contains(t, supported.Value(), "siprec")

	to := resp.To()
	require.NotNil(t, to)
	_, hasTag := to.Params.Get("tag")
	assert.True(t, hasTag, "To header should have a tag")

	assert.Equal(t, combinedSDP, string(resp.Body()))
}

// =============================================================================
// Helper Function Tests
// =============================================================================

func TestGenerateSiprecTag(t *testing.T) {
	tag1 := generateSiprecTag()
	tag2 := generateSiprecTag()

	assert.NotEmpty(t, tag1)
	assert.NotEmpty(t, tag2)
	assert.NotEqual(t, tag1, tag2)
	assert.True(t, isHexString(tag1))
	assert.True(t, isHexString(tag2))
}

func isHexString(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
