//go:build !kiss

package radio

func kissAvailable() bool                 { return false }
func newKISS(cfg Config) (decoder, error) { return nil, &UnavailableError{Kind: cfg.Kind} }
func newKISSTransmitter(cfg Config) (transmitter, error) {
	return nil, &UnavailableError{Kind: cfg.Kind}
}
