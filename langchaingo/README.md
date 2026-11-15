# flyedge langchaingo messages

Converts a reverse-invocation conversation into langchaingo messages, so a stateless handler
passes the whole conversation to the model instead of only the latest user message.

It lives in its own module so the core `flyedge` library stays dependency-free.

```go
import felc "github.com/compfly-ai/flyedge-go/langchaingo"

conn, err := guard.ServeInvocations(ctx, options, func(ctx context.Context, request flyedge.InvocationRequest, w flyedge.InvocationWriter) error {
	messages, err := felc.Messages(request)
	if err != nil {
		return err
	}
	reply, err := llm.GenerateContent(ctx, messages)
	if err != nil {
		return err
	}
	return w.TextDelta(ctx, reply.Choices[0].Content)
})
```

`Messages` puts supplied `History` before the live `Messages` and maps user, assistant
(including tool calls) and tool messages. Other roles, including `system`, are rejected:
system instructions stay owned by the application. Recorded tool calls are context; never
execute them again.
