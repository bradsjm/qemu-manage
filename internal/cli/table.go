package cli

import (
	"io"
	"strings"

	"github.com/pterm/pterm"
)

// writeTable renders headers and rows with pterm's default table printer.
func writeTable(output io.Writer, interactive bool, headers []string, rows [][]string) error {
	output = normalizeOutput(output)
	data := make(pterm.TableData, 0, len(rows)+1)
	data = append(data, append([]string(nil), headers...))
	for _, row := range rows {
		data = append(data, append([]string(nil), row...))
	}

	rendered, err := pterm.DefaultTable.WithHasHeader().WithData(data).Srender()
	if err != nil {
		return err
	}
	if !interactive {
		rendered = pterm.RemoveColorFromString(rendered)
	}
	rendered = strings.TrimRight(rendered, "\n") + "\n"

	_, err = io.WriteString(output, rendered)
	return err
}
