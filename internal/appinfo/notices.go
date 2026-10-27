package appinfo

import _ "embed"

// Notices accompanies every binary with its dependency licenses.
//
//go:embed notices.txt
var Notices string
