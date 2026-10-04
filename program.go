package main

import (
	"fmt"
	"strings"
)

// The uploader runs at FRC events (FMS, The Blue Alliance) and at FTC events
// (FTC Live, The Orange Alliance). FIM-AV Assistant says which with -program,
// and the program decides three things: which recording names parse (see
// parse_ftc.go), where scores come from (FMS via FIM-AV, or FTC Live), and where
// match videos are submitted (TBA or TOA).
const (
	programFRC = "frc"
	programFTC = "ftc"
)

// program is set once from the -program flag in main(). FRC is the default so
// every existing launch keeps its behaviour.
var program = programFRC

// isFTC reports whether this process is running for an FTC event.
func isFTC() bool { return program == programFTC }

// parseProgram validates the -program flag value.
func parseProgram(v string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", programFRC:
		return programFRC, nil
	case programFTC:
		return programFTC, nil
	}
	return "", fmt.Errorf("unknown -program %q (want frc or ftc)", v)
}
