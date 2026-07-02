package llmgateway

import _ "embed"

//go:embed config/systemd.service
var SystemdService []byte

//go:embed config/systemd.cuda.service
var SystemdServiceCUDA []byte
