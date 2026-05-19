package setup

import _ "embed"

//go:embed hospital-sidecar.service
var systemdUnit []byte
