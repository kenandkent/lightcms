// Package reportstatic embeds the Worker modules. The CMS server does not need
// Node or Wrangler at runtime; routes.json is generated from a frozen release.
package reportstatic

import (
	"embed"
	"encoding/json"
	"errors"
)

//go:embed src/index.js src/handler.mjs
var source embed.FS

type Module struct {
	Name, MIME string
	Data       []byte
}

func Modules(routeJSON []byte) ([]Module, error) {
	if !json.Valid(routeJSON) {
		return nil, errors.New("invalid Worker route JSON")
	}
	index, e := source.ReadFile("src/index.js")
	if e != nil {
		return nil, e
	}
	handler, e := source.ReadFile("src/handler.mjs")
	if e != nil {
		return nil, e
	}
	if len(index)+len(handler)+len(routeJSON) > 3<<20 {
		return nil, errors.New("Worker modules exceed conservative plan limit")
	}
	return []Module{{Name: "index.js", MIME: "application/javascript+module", Data: index}, {Name: "handler.mjs", MIME: "application/javascript+module", Data: handler}, {Name: "routes.json", MIME: "text/plain", Data: append([]byte(nil), routeJSON...)}}, nil
}
