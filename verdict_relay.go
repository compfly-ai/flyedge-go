package flyedge

import "context"

type verdictRelayKey struct{}

// WithVerdictRelay describes whether this host can apply a verdict. It does not
// alter the verdict. Passive/unsupported stages must never count as blocked.
func WithVerdictRelay(ctx context.Context, relay string) context.Context {
	switch relay {
	case "relayed", "host_cannot_block", "passive_stage", "stream_delivered", "ungoverned_stage":
	default:
		relay = "host_cannot_block"
	}
	return context.WithValue(ctx, verdictRelayKey{}, relay)
}
func verdictRelay(ctx context.Context) string {
	if value, ok := ctx.Value(verdictRelayKey{}).(string); ok {
		return value
	}
	return "relayed"
}
