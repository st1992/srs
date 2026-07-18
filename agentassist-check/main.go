// Command agentassist-check is a standalone smoke test for Dialogflow's
// BidiStreamingAnalyzeContent API. It exercises the exact same request shape
// as agentassist.go (CreateConversation -> CreateParticipant -> bidi stream
// with a Config message followed by audio chunks) without any of the SIP,
// RTP, or Kubernetes machinery, so a conversation profile or IAM/network
// issue can be isolated in seconds instead of a full rebuild+redeploy cycle.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	dialogflow "cloud.google.com/go/dialogflow/apiv2beta1"
	"cloud.google.com/go/dialogflow/apiv2beta1/dialogflowpb"
	"google.golang.org/api/option"
)

func main() {
	project := flag.String("project", "", "GCP project ID (required)")
	location := flag.String("location", "global", "Dialogflow location, e.g. us-central1 or global")
	profile := flag.String("profile", "", "Conversation profile ID (required)")
	role := flag.String("role", "HUMAN_AGENT", "Participant role: HUMAN_AGENT or END_USER")
	encoding := flag.String("encoding", "MULAW", "Audio encoding: MULAW or LINEAR16")
	sampleRate := flag.Int("sample-rate", 8000, "Audio sample rate (Hz)")
	audioFile := flag.String("audio-file", "", "Raw audio bytes to stream in the given encoding; omit to stream silence")
	numChunks := flag.Int("chunks", 25, "Number of ~20ms silence chunks to send when -audio-file is omitted")
	credentialsFile := flag.String("credentials-file", "", "Path to a GCP service account JSON key; omit to use Application Default Credentials / Workload Identity")
	flag.Parse()

	if *project == "" || *profile == "" {
		fmt.Fprintln(os.Stderr, "usage: agentassist-check -project=<id> -profile=<id> [-location=us-central1] [-role=HUMAN_AGENT|END_USER] [-encoding=MULAW|LINEAR16] [-sample-rate=8000] [-audio-file=path] [-credentials-file=path]")
		os.Exit(2)
	}

	ctx := context.Background()

	var opts []option.ClientOption
	if *credentialsFile != "" {
		opts = append(opts, option.WithCredentialsFile(*credentialsFile))
	}
	if *location != "" && *location != "global" {
		opts = append(opts, option.WithEndpoint(fmt.Sprintf("%s-dialogflow.googleapis.com:443", *location)))
	}

	conversations, err := dialogflow.NewConversationsClient(ctx, opts...)
	if err != nil {
		log.Fatalf("create conversations client: %v", err)
	}
	defer conversations.Close()

	participants, err := dialogflow.NewParticipantsClient(ctx, opts...)
	if err != nil {
		log.Fatalf("create participants client: %v", err)
	}
	defer participants.Close()

	parent := fmt.Sprintf("projects/%s/locations/%s", *project, *location)
	profileName := fmt.Sprintf("%s/conversationProfiles/%s", parent, *profile)

	log.Printf("creating conversation under profile %s", profileName)
	conv, err := conversations.CreateConversation(ctx, &dialogflowpb.CreateConversationRequest{
		Parent:       parent,
		Conversation: &dialogflowpb.Conversation{ConversationProfile: profileName},
	})
	if err != nil {
		log.Fatalf("create conversation: %v", err)
	}
	log.Printf("conversation created: %s", conv.Name)
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := conversations.CompleteConversation(cctx, &dialogflowpb.CompleteConversationRequest{Name: conv.Name}); err != nil {
			log.Printf("complete conversation: %v", err)
		}
	}()

	participantRole := dialogflowpb.Participant_HUMAN_AGENT
	if *role == "END_USER" {
		participantRole = dialogflowpb.Participant_END_USER
	}

	participant, err := participants.CreateParticipant(ctx, &dialogflowpb.CreateParticipantRequest{
		Parent:      conv.Name,
		Participant: &dialogflowpb.Participant{Role: participantRole},
	})
	if err != nil {
		log.Fatalf("create participant: %v", err)
	}
	log.Printf("participant created: %s (role=%s)", participant.Name, participantRole)

	stream, err := participants.BidiStreamingAnalyzeContent(ctx)
	if err != nil {
		log.Fatalf("open bidi stream: %v", err)
	}

	audioEncoding := dialogflowpb.AudioEncoding_AUDIO_ENCODING_MULAW
	if *encoding == "LINEAR16" {
		audioEncoding = dialogflowpb.AudioEncoding_AUDIO_ENCODING_LINEAR_16
	}

	log.Printf("sending config: encoding=%s sample_rate=%d", audioEncoding, *sampleRate)
	if err := stream.Send(&dialogflowpb.BidiStreamingAnalyzeContentRequest{
		Request: &dialogflowpb.BidiStreamingAnalyzeContentRequest_Config_{
			Config: &dialogflowpb.BidiStreamingAnalyzeContentRequest_Config{
				Participant: participant.Name,
				Config: &dialogflowpb.BidiStreamingAnalyzeContentRequest_Config_VoiceSessionConfig_{
					VoiceSessionConfig: &dialogflowpb.BidiStreamingAnalyzeContentRequest_Config_VoiceSessionConfig{
						InputAudioEncoding:        audioEncoding,
						InputAudioSampleRateHertz: int32(*sampleRate),
					},
				},
			},
		},
	}); err != nil {
		log.Fatalf("send config: %v", err)
	}

	recvDone := make(chan struct{})
	go func() {
		defer close(recvDone)
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				log.Println("RECV: stream closed by server (EOF)")
				return
			}
			if err != nil {
				log.Printf("RECV ERROR: %v", err)
				return
			}
			if result := resp.GetRecognitionResult(); result != nil {
				log.Printf("RECV: transcript=%q is_final=%v", result.GetTranscript(), result.GetIsFinal())
				continue
			}
			log.Printf("RECV: %+v", resp)
		}
	}()

	var audio []byte
	if *audioFile != "" {
		data, err := os.ReadFile(*audioFile)
		if err != nil {
			log.Fatalf("read audio file: %v", err)
		}
		audio = data
	}

	chunkSize := *sampleRate / 50 // ~20ms per chunk at 1 byte/sample (MULAW/LINEAR8)
	if chunkSize <= 0 {
		chunkSize = 160
	}

	sendChunk := func(chunk []byte) error {
		return stream.Send(&dialogflowpb.BidiStreamingAnalyzeContentRequest{
			Request: &dialogflowpb.BidiStreamingAnalyzeContentRequest_Input_{
				Input: &dialogflowpb.BidiStreamingAnalyzeContentRequest_Input{
					Input: &dialogflowpb.BidiStreamingAnalyzeContentRequest_Input_Audio{Audio: chunk},
				},
			},
		})
	}

	if len(audio) > 0 {
		log.Printf("streaming %d bytes from %s in %d-byte chunks", len(audio), *audioFile, chunkSize)
		for i := 0; i < len(audio); i += chunkSize {
			end := i + chunkSize
			if end > len(audio) {
				end = len(audio)
			}
			if err := sendChunk(audio[i:end]); err != nil {
				log.Printf("SEND ERROR at offset %d: %v", i, err)
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	} else {
		log.Printf("streaming %d chunks of silence", *numChunks)
		silence := make([]byte, chunkSize)
		for i := range silence {
			silence[i] = 0xFF // MULAW silence; harmless as LINEAR16 too (near-zero amplitude)
		}
		for i := 0; i < *numChunks; i++ {
			if err := sendChunk(silence); err != nil {
				log.Printf("SEND ERROR on chunk %d: %v", i, err)
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	if err := stream.CloseSend(); err != nil {
		log.Printf("close send: %v", err)
	}

	select {
	case <-recvDone:
	case <-time.After(10 * time.Second):
		log.Println("timed out waiting for the receive loop to finish")
	}

	log.Println("done")
}
