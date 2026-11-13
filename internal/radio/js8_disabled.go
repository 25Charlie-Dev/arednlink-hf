//go:build !js8call

package radio

func js8Available() bool                 { return false }
func newJS8(cfg Config) (decoder, error) { return nil, &UnavailableError{Kind: cfg.Kind} }
func newJS8Transmitter(cfg Config) (transmitter, error) {
	return nil, &UnavailableError{Kind: cfg.Kind}
}
