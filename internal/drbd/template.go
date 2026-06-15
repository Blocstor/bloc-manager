package drbd

import (
	"bytes"
	"fmt"
	"text/template"
)

const resTemplate = `resource {{ .Name }} {
  net {
    protocol C;
  }
  disk {
    on-io-error detach;
  }
  options {
    quorum off;
    on-no-quorum io-error;
    auto-promote yes;
  }
  volume 0 {
    device minor {{ .Minor }};
    disk /dev/{{ .VG }}/{{ .LVName }};
    meta-disk internal;
  }
{{ range .Nodes }}  on {{ .Hostname }} {
    address {{ .Address }}:{{ .Port }};
  }
{{ end }}}
`

// ResNode holds the per-node addressing information for a DRBD resource.
type ResNode struct {
	Hostname string
	Address  string
	Port     int
}

// ResData contains all fields required to render a DRBD .res file.
type ResData struct {
	Name   string
	VG     string
	LVName string
	Minor  int
	Nodes  []ResNode
}

var resTmpl = template.Must(template.New("res").Parse(resTemplate))

// RenderRes renders a DRBD .res file from the provided data.
func RenderRes(data ResData) (string, error) {
	var buf bytes.Buffer
	if err := resTmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render res template: %w", err)
	}
	return buf.String(), nil
}
