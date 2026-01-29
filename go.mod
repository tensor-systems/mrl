module github.com/modelrelay/modelrelay/cmd/mrl

go 1.25.4

require (
	github.com/BurntSushi/toml v1.4.0
	github.com/google/uuid v1.6.0
	github.com/modelrelay/modelrelay/platform v0.0.0-00010101000000-000000000000
	github.com/modelrelay/modelrelay/providers v0.0.0-20251119210239-1133abe831c1
	github.com/modelrelay/modelrelay/sdk/go v0.0.0
	github.com/spf13/cobra v1.10.2
	github.com/spf13/pflag v1.0.10
)

require (
	github.com/apapsch/go-jsonmerge/v2 v2.0.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/evanphx/json-patch/v5 v5.9.11 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/oapi-codegen/runtime v1.1.2 // indirect
	github.com/santhosh-tekuri/jsonschema/v5 v5.3.1 // indirect
	go.opentelemetry.io/otel v1.39.0 // indirect
	go.opentelemetry.io/otel/trace v1.39.0 // indirect
)

replace github.com/modelrelay/modelrelay/sdk/go => ../../sdk/go

replace github.com/modelrelay/modelrelay/platform => ../../platform

replace github.com/modelrelay/modelrelay/providers => ../../providers

replace github.com/modelrelay/modelrelay/billing => ../../billing
