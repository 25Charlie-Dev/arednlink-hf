// Package control serves bounded local daemon requests.
package control

import "context"

type ConfigSnapshot struct {
	Running  string `json:"running"`
	Saved    string `json:"saved"`
	Disabled string `json:"save_disabled"`
	Pending  bool   `json:"pending"`
}
type ConfigBackend interface {
	Snapshot(context.Context) (ConfigSnapshot, error)
	// Disabled returns the reason saving is unavailable, or "" when it is allowed.
	// It never reads or decodes the saved file.
	Disabled() string
	Validate(context.Context, string) error
	Save(context.Context, string) error
}
