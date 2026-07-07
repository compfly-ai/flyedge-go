module github.com/compfly-ai/flyedge-go/examples

go 1.26.4

replace github.com/compfly-ai/flyedge-go => ../

require (
	github.com/anthropics/anthropic-sdk-go v1.56.0
	github.com/compfly-ai/flyedge-go v0.0.0-00010101000000-000000000000
	github.com/compfly-ai/flyedge-go/telemetry/otel v0.0.0-00010101000000-000000000000
	github.com/openai/openai-go v1.12.0
	github.com/tmc/langchaingo v0.1.14
	go.opentelemetry.io/otel v1.44.0
	go.opentelemetry.io/otel/exporters/stdout/stdouttrace v1.44.0
	go.opentelemetry.io/otel/sdk v1.44.0
)

require (
	github.com/bahlo/generic-list-go v0.2.0 // indirect
	github.com/buger/jsonparser v1.1.2 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dlclark/regexp2 v1.10.0 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/invopop/jsonschema v0.14.0 // indirect
	github.com/pb33f/ordered-map/v2 v2.3.1 // indirect
	github.com/pkoukk/tiktoken-go v0.1.6 // indirect
	github.com/standard-webhooks/standard-webhooks/libraries v0.0.1 // indirect
	github.com/tidwall/gjson v1.18.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/metric v1.44.0 // indirect
	go.opentelemetry.io/otel/trace v1.44.0 // indirect
	go.yaml.in/yaml/v4 v4.0.0-rc.2 // indirect
	golang.org/x/sync v0.16.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
)

replace github.com/compfly-ai/flyedge-go/telemetry/otel => ../telemetry/otel
