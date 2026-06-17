package llmgateway

import _ "embed"

//go:embed config/systemd.service
var SystemdService []byte
