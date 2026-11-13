package core

import (
	"fmt"
	"net/netip"
	"time"
)

// ValidateConfig checks configuration bounds and destination policy without I/O.
func ValidateConfig(cfg Config) error {
	_, err := normalizeConfig(cfg)
	return err
}

func normalizeConfig(cfg Config) (Config, error) {
	if cfg.MaxPeers == 0 {
		cfg.MaxPeers = 256
	}
	if cfg.FullPolicy == "" {
		cfg.FullPolicy = FullPolicyReplace
	}
	if cfg.FullPolicy != FullPolicyReplace && cfg.FullPolicy != FullPolicyRefuse {
		return Config{}, fmt.Errorf("%w: unknown full policy", ErrInvalidConfig)
	}
	if cfg.ProbeTimeout == 0 {
		cfg.ProbeTimeout = 3 * time.Second
	}
	if cfg.ProbeInterval == 0 {
		cfg.ProbeInterval = time.Minute
	}
	if cfg.ProbeGlobalInterval == 0 {
		cfg.ProbeGlobalInterval = 10 * time.Second
	}
	if err := checkBounds(cfg); err != nil {
		return Config{}, err
	}
	if err := validatePolicy(cfg); err != nil {
		return Config{}, err
	}
	cfg.ProbePrefixes = append([]netip.Prefix(nil), cfg.ProbePrefixes...)
	cfg.ProbePorts = append([]uint16(nil), cfg.ProbePorts...)
	return cfg, nil
}

// checkBounds enforces storage, timeout and destination-policy limits on a defaulted config.
func checkBounds(cfg Config) error {
	if cfg.MaxPeers < 1 || cfg.MaxPeers > 4096 || cfg.ProbeTimeout <= 0 || cfg.ProbeTimeout > 30*time.Second {
		return fmt.Errorf("%w: storage or timeout bounds", ErrInvalidConfig)
	}
	if cfg.ProbeGlobalInterval < 10*time.Second || cfg.ProbeGlobalInterval > 24*time.Hour ||
		cfg.ProbeInterval < cfg.ProbeGlobalInterval || cfg.ProbeInterval > 24*time.Hour {
		return fmt.Errorf("%w: probe interval bounds", ErrInvalidConfig)
	}
	if len(cfg.ProbePrefixes) > 128 || len(cfg.ProbePorts) > 128 {
		return fmt.Errorf("%w: destination policy bounds", ErrInvalidConfig)
	}
	if cfg.EnableProbes && (len(cfg.ProbePrefixes) == 0 || len(cfg.ProbePorts) == 0) {
		return fmt.Errorf("%w: enabled probes require prefixes and ports", ErrInvalidConfig)
	}
	return nil
}

func validatePolicy(cfg Config) error {
	for _, prefix := range cfg.ProbePrefixes {
		if !prefix.IsValid() || prefix.Addr().Is4In6() || prefix.Addr().Zone() != "" {
			return fmt.Errorf("%w: invalid destination prefix", ErrInvalidConfig)
		}
	}
	for _, port := range cfg.ProbePorts {
		if port == 0 {
			return fmt.Errorf("%w: zero destination port", ErrInvalidConfig)
		}
	}
	return nil
}
