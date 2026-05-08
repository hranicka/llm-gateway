package llmgateway

import _ "embed"

//go:embed config/example.yaml
var ExampleYAML []byte

//go:embed config/systemd.service
var SystemdService []byte
