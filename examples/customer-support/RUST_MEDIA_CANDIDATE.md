# Consolidated Rust SDK candidate

This task branch changes only the local customer-support Go demo to use the
new `compfly-ai/flyedge` Go binding. The binding is still in draft PR #153;
the module version in `go.mod` is a local build placeholder. Build with the
matching SDK checkout as a named Docker context:

```sh
docker buildx build --load \
  --build-context sdk_context=/absolute/path/to/flyedge-checkout \
  -f examples/customer-support/Dockerfile.rust-media-candidate \
  -t compfly/customer-support-go:rust-media-local .
```

The candidate passed real local text, PNG, PDF, and `lookup_order` requests
using SDK commit `36f780c`. The local Audit Logs recorded decoded PNG/PDF pre-LLM decisions and
post-LLM decisions for those sessions. The original Tilt Go deployment was
restored after testing. This branch is a checkpoint, not a replacement image
or deployment instruction. User E2E and SDK release approval remain pending.
