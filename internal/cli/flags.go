package cli

import "strings"

// EventFlags collects repeated --event values.
type EventFlags []string

func (e *EventFlags) String() string {
	if e == nil {
		return ""
	}
	return strings.Join(*e, ",")
}

func (e *EventFlags) Set(v string) error {
	*e = append(*e, v)
	return nil
}
