package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	dialogflow "cloud.google.com/go/dialogflow/apiv2beta1"
	"cloud.google.com/go/dialogflow/apiv2beta1/dialogflowpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestParseAgentAssistTarget(t *testing.T) {
	tests := []struct {
		name, profile, region string
		want                  agentAssistTarget
		lone                  bool
		wantErr               string
	}{
		{name: "both", profile: "prof_A-1", region: "us-central1", want: agentAssistTarget{"prof_A-1", "us-central1"}},
		{name: "normalized", profile: " p ", region: " US-Central1 ", want: agentAssistTarget{"p", "us-central1"}},
		{name: "neither"},
		{name: "whitespace only", profile: " ", region: "\t"},
		{name: "lone profile", profile: "p", lone: true},
		{name: "lone region", region: "us", profile: "  ", lone: true},
		{name: "lone malformed value is ignored too", region: "evil.com/", lone: true},
		{name: "lone full name is ignored too", profile: "projects/x", lone: true},
		{name: "region slash", profile: "p", region: "evil.com/", wantErr: "aa_region"},
		{name: "region dot", profile: "p", region: "a.b", wantErr: "aa_region"},
		{name: "region port", profile: "p", region: "x:1", wantErr: "aa_region"},
		{name: "region trailing hyphen", profile: "p", region: "us-", wantErr: "aa_region"},
		{name: "region too long", profile: "p", region: strings.Repeat("a", 53), wantErr: "aa_region"},
		{name: "full resource name", profile: "projects/p/conversationProfiles/x", region: "us", wantErr: "aa_conversation_profile_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, lone, err := parseAgentAssistTarget(tt.profile, tt.region)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.lone, lone)
		})
	}
}

func TestAgentAssistAPI_StartTarget(t *testing.T) {
	tests := []struct {
		name, fields string
		status       int
		want         agentAssistTarget
	}{
		{"legacy body", ``, 200, agentAssistTarget{}},
		{"both", `,"aa_conversation_profile_id":"p2","aa_region":"europe-west2"`, 200, agentAssistTarget{"p2", "europe-west2"}},
		{"lone profile", `,"aa_conversation_profile_id":"p2"`, 200, agentAssistTarget{}},
		{"lone region", `,"aa_region":"us"`, 200, agentAssistTarget{}},
		{"malformed", `,"aa_conversation_profile_id":"p2","aa_region":"evil.com/"`, 400, agentAssistTarget{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, sess := newTestSplitServerWithCallID(t, "call-1")
			fake := &fakeAgentAssistClient{}
			srv.assist = fake
			cfg := *srv.cfg
			api := NewAPIServer(&cfg, srv, testLogger())
			start := func(fields string) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				api.server.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/agent-assist/start",
					strings.NewReader(`{"call_id":"call-1"`+fields+`}`)))
				return rec
			}

			rec := start(tt.fields)
			require.Equal(t, tt.status, rec.Code, rec.Body.String())
			assert.Equal(t, tt.want, fake.lastReq.Target)
			if tt.status != http.StatusOK {
				assert.Zero(t, fake.starts)
				for _, leg := range sess.Legs {
					assert.Equal(t, "recording", leg.SinkKind())
				}
				return
			}

			again := start(`,"aa_conversation_profile_id":"p3","aa_region":"us"`)
			assert.Equal(t, http.StatusOK, again.Code)
			assert.JSONEq(t, rec.Body.String(), again.Body.String())
			assert.Equal(t, 1, fake.starts, "a repeat start keeps the running conversation")
		})
	}
}

func TestClientsFor(t *testing.T) {
	var mu sync.Mutex
	built := map[string]int{}
	c := &googleAgentAssistClient{cfg: &Config{}, log: testLogger()}
	c.newClients = func(_ context.Context, location string) (*dialogflowClients, error) {
		mu.Lock()
		defer mu.Unlock()
		built[location]++
		if location == "flaky" && built[location] == 1 {
			return nil, errors.New("boom")
		}
		return &dialogflowClients{}, nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.clientsFor(context.Background(), []string{"us", "global"}[i%2])
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	assert.Equal(t, map[string]int{"us": 1, "global": 1}, built)
	assert.NotSame(t, c.clients["us"], c.clients["global"])

	_, err := c.clientsFor(context.Background(), "flaky")
	require.Error(t, err)
	_, err = c.clientsFor(context.Background(), "flaky")
	require.NoError(t, err, "a failed construction must not be cached")
}

// fakeDialogflow is an in-process Dialogflow endpoint for one region.
type fakeDialogflow struct {
	dialogflowpb.UnimplementedConversationsServer
	dialogflowpb.UnimplementedParticipantsServer

	hang bool // CreateConversation blocks until its ctx is done

	mu    sync.Mutex
	calls []string
}

func (f *fakeDialogflow) record(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *fakeDialogflow) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeDialogflow) CreateConversation(ctx context.Context, r *dialogflowpb.CreateConversationRequest) (*dialogflowpb.Conversation, error) {
	if f.hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	f.record("create %s", r.Conversation.ConversationProfile)
	return &dialogflowpb.Conversation{Name: r.Parent + "/conversations/c1"}, nil
}

func (f *fakeDialogflow) CompleteConversation(_ context.Context, r *dialogflowpb.CompleteConversationRequest) (*dialogflowpb.Conversation, error) {
	f.record("complete %s", r.Name)
	return &dialogflowpb.Conversation{Name: r.Name}, nil
}

func (f *fakeDialogflow) CreateParticipant(_ context.Context, r *dialogflowpb.CreateParticipantRequest) (*dialogflowpb.Participant, error) {
	f.record("participant %s", r.Parent)
	return &dialogflowpb.Participant{Name: r.Parent + "/participants/p1"}, nil
}

func (f *fakeDialogflow) BidiStreamingAnalyzeContent(s dialogflowpb.Participants_BidiStreamingAnalyzeContentServer) error {
	for {
		req, err := s.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if cfg := req.GetConfig(); cfg != nil {
			f.record("stream %s", cfg.Participant)
		}
	}
}

func serveFakeDialogflow(t *testing.T, f *fakeDialogflow) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	s := grpc.NewServer()
	dialogflowpb.RegisterConversationsServer(s, f)
	dialogflowpb.RegisterParticipantsServer(s, f)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)
	return lis.Addr().String()
}

func newFakeDialogflowClient(t *testing.T, regions map[string]*fakeDialogflow) *googleAgentAssistClient {
	t.Helper()
	addrs := map[string]string{}
	for region, f := range regions {
		addrs[region] = serveFakeDialogflow(t, f)
	}
	c := &googleAgentAssistClient{log: testLogger(), cfg: &Config{
		AgentAssistProjectID:                "proj",
		AgentAssistLocationID:               "global",
		AgentAssistConversationProfileID:    "default",
		AgentAssistSampleRateHertz:          8000,
		AgentAssistSendQueuePackets:         10,
		AgentAssistStreamRotateAfterSeconds: 780,
		AgentAssistStreamHardLimitSeconds:   825,
		AgentAssistStreamIdleMillis:         800,
	}}
	c.newClients = func(ctx context.Context, location string) (*dialogflowClients, error) {
		conn := func() option.ClientOption {
			cc, err := grpc.NewClient(addrs[location], grpc.WithTransportCredentials(insecure.NewCredentials()))
			require.NoError(t, err)
			return option.WithGRPCConn(cc)
		}
		conversations, err := dialogflow.NewConversationsClient(ctx, conn())
		if err != nil {
			return nil, err
		}
		participants, err := dialogflow.NewParticipantsClient(ctx, conn())
		if err != nil {
			return nil, err
		}
		return &dialogflowClients{conversations: conversations, participants: participants}, nil
	}
	t.Cleanup(func() { assert.NoError(t, c.Close()) })
	return c
}

// A conversation must stay on its own region's endpoint from creation through
// CompleteConversation, including streams opened after the request is gone.
func TestGoogleStart_UsesTargetRegionThroughout(t *testing.T) {
	for _, tt := range []struct {
		name, region, profile string
		target                agentAssistTarget
	}{
		{"override", "europe-west2", "other", agentAssistTarget{"other", "europe-west2"}},
		{"configured default", "global", "default", agentAssistTarget{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			regions := map[string]*fakeDialogflow{"global": {}, "europe-west2": {}}
			c := newFakeDialogflowClient(t, regions)

			ctx, cancel := context.WithCancel(context.Background())
			run, err := c.Start(ctx, AgentAssistStartRequest{
				CallID: "call-1", Labels: []string{"inbound", "outbound"}, Target: tt.target,
				OnStreamError: func(err error) { t.Errorf("stream error: %v", err) },
			})
			require.NoError(t, err)
			cancel() // the HTTP request ends; the conversation must not
			_, err = run.Sinks["inbound"].(*agentAssistSink).open()
			require.NoError(t, err, "a rotation must still be able to open a stream")

			f := regions[tt.region]
			require.Eventually(t, func() bool { return len(f.recorded()) == 6 }, 5*time.Second, 5*time.Millisecond)
			require.NoError(t, run.complete(context.Background()))

			parent := "projects/proj/locations/" + tt.region
			conv := parent + "/conversations/c1"
			assert.ElementsMatch(t, []string{
				"create " + parent + "/conversationProfiles/" + tt.profile,
				"participant " + conv, "participant " + conv,
				"stream " + conv + "/participants/p1", "stream " + conv + "/participants/p1", "stream " + conv + "/participants/p1",
				"complete " + conv,
			}, f.recorded())
			for region, other := range regions {
				if region != tt.region {
					assert.Empty(t, other.recorded(), "%s must not be contacted", region)
				}
			}
		})
	}
}

func TestGoogleStart_SetupTimeout(t *testing.T) {
	old := agentAssistSetupTimeout
	agentAssistSetupTimeout = 100 * time.Millisecond
	t.Cleanup(func() { agentAssistSetupTimeout = old })

	c := newFakeDialogflowClient(t, map[string]*fakeDialogflow{"global": {hang: true}})
	began := time.Now()
	_, err := c.Start(context.Background(), AgentAssistStartRequest{CallID: "call-1", Labels: []string{"inbound"}})
	require.Error(t, err)
	assert.Less(t, time.Since(began), 5*time.Second)
}
