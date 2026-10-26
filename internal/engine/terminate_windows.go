package engine

import "os"

// Windows has no SIGTERM; the console Ctrl+C already reached the child.
func terminate(p *os.Process) error { return p.Kill() }
