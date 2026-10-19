module github.com/example/dbx-plugin-kubecontext/backend

go 1.22

require (
	github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk v0.1.0
	gopkg.in/yaml.v3 v3.0.1
)

require github.com/creack/pty v1.1.24

// The DBX Go SDK is vendored so the project builds without network access and
// without a local DBX checkout. Drop this replace line (and re-run `go mod tidy`)
// to follow the published SDK, or run `dbx-plugin dev --sdk-root <dbx>` instead.
replace github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk => ./third_party/dbx-plugin-sdk
