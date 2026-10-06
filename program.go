package main

import (
	"fmt"
	"strings"
	"sync"
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

// program is set from the -program flag in main(), and can be switched live by
// POST /api/control/event. FRC is the default so every existing launch keeps
// its behaviour. Read it through currentProgram/isFTC; the upload loop reads it
// from its own goroutine.
var (
	programMu sync.RWMutex
	program   = programFRC
)

// currentProgram returns the program in use.
func currentProgram() string {
	programMu.RLock()
	defer programMu.RUnlock()
	return program
}

// setProgram switches the program. The value must come from parseProgram.
func setProgram(p string) {
	programMu.Lock()
	program = p
	programMu.Unlock()
}

// isFTC reports whether this process is running for an FTC event.
func isFTC() bool { return currentProgram() == programFTC }

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
