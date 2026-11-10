package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	flyedge "github.com/compfly-ai/flyedge-go"
)

func reverseOptions(environment string) flyedge.InvocationOptions {
	enabled := true
	return flyedge.InvocationOptions{
		Name: "Reference concierge", Environment: environment,
		Purposes:           []flyedge.InvocationPurpose{flyedge.PurposePlayground, flyedge.PurposeSimulation, flyedge.PurposeEval},
		AllowClientHistory: true, AllowSimulationInjection: &enabled,
		AllowInsecureLoopback: os.Getenv("COMPFLY_ALLOW_INSECURE_LOOPBACK") == "true",
		Contract: &flyedge.InvocationContract{State: flyedge.StateContract{Fields: map[string]flyedge.StateField{
			"location": {Type: flyedge.StateString, PlaygroundDefault: json.RawMessage(`"San Francisco, CA"`)},
		}}},
	}
}

// Stateless composition: preserve typed history without replaying historical calls.
// Identity always comes from the application's selected user, never caller state.
func reverseConversation(req flyedge.InvocationRequest) ([]flyedge.InvocationMessage, string, error) {
	var location string
	for key, raw := range req.State {
		if key != "location" {
			return nil, "", fmt.Errorf("unsupported state field %q", key)
		}
		if err := json.Unmarshal(raw, &location); err != nil || strings.TrimSpace(location) == "" || len(location) > 200 {
			return nil, "", fmt.Errorf("location must be a nonempty string of at most 200 bytes")
		}
	}
	if len(req.Messages) == 0 || req.Messages[len(req.Messages)-1].Role != "user" || strings.TrimSpace(req.Messages[len(req.Messages)-1].Content) == "" {
		return nil, "", fmt.Errorf("a final user message is required")
	}
	messages := []flyedge.InvocationMessage{}
	if req.History != nil {
		messages = append(messages, (*req.History)...)
	}
	messages = append(messages, req.Messages...)
	return messages, location, nil
}

func serveReverse(ctx context.Context, a *agent, u *user, environment string) error {
	conn, err := a.guard.ServeInvocations(ctx, reverseOptions(environment), func(ctx context.Context, req flyedge.InvocationRequest, out flyedge.InvocationWriter) error {
		messages, location, err := reverseConversation(req)
		if err != nil {
			return err
		}
		reply, err := a.handleConversation(ctx, u, req.SessionID, messages, location)
		if err != nil {
			return err
		}
		return out.TextDelta(ctx, reply.text)
	})
	if err != nil {
		return err
	}
	defer conn.Close()
	fmt.Fprintf(os.Stderr, "reverse invocation ready: environment=%s; select this agent in Playground\n", environment)
	return conn.Wait()
}
