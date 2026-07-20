package supervisor

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/bradsjm/qemu-manage/internal/backend"
)

// debugf writes line-prefixed debug output when debug logging is enabled.
func debugf(enabled bool, output io.Writer, format string, args ...any) {
	if !enabled || output == nil {
		return
	}
	message := fmt.Sprintf(format, args...)
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	for _, line := range strings.SplitAfter(message, "\n") {
		if line == "" {
			continue
		}
		if _, err := io.WriteString(output, "debug: "); err != nil {
			return
		}
		if _, err := io.WriteString(output, line); err != nil {
			return
		}
	}
}

// formatQuotedArgv shell-quotes a command line for debug output
func formatQuotedArgv(path string, args []string) string {
	quoted := make([]string, 0, len(args)+1)
	quoted = append(quoted, strconv.Quote(path))
	for _, arg := range args {
		quoted = append(quoted, strconv.Quote(arg))
	}
	return strings.Join(quoted, " ")
}

// formatManagedCommand omits supervisor-injected trailing arguments from debug logs
func formatManagedCommand(command backend.Command, extraArgsCount int) string {
	if extraArgsCount < 0 || extraArgsCount > len(command.Args) {
		extraArgsCount = 0
	}
	managed := command.Args[:len(command.Args)-extraArgsCount]
	return formatQuotedArgv(command.Path, managed)
}
