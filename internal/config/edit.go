package config

import "fmt"

type EditAckMode string

const (
	EditAckFull  EditAckMode = "full"
	EditAckLocal EditAckMode = "local"
)

// EditConfig controls the source mutation reply policy. Empty retains the
// shipped full mode; local is always an explicit operator choice.
type EditConfig struct {
	Ack EditAckMode `mapstructure:"ack" yaml:"ack,omitempty"`
}

func (e EditConfig) AckMode() (EditAckMode, error) {
	switch e.Ack {
	case "", EditAckFull:
		return EditAckFull, nil
	case EditAckLocal:
		return EditAckLocal, nil
	default:
		return "", fmt.Errorf("config: edit.ack must be full or local, got %q", e.Ack)
	}
}
