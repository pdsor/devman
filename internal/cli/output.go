package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// printJSON writes a value as indented JSON.
//
// JSON is the contract; the tables below are a rendering of the same DTOs. That
// order matters: it is why `--json` is never a second-class output.
func (a *App) printJSON(value any) {
	encoder := json.NewEncoder(a.Stdout)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		fmt.Fprintf(a.Stderr, "devman: cannot encode output: %v\n", err)
	}
}

// printNDJSON writes one object per line, for streaming output an agent or a
// shell pipeline consumes incrementally.
func (a *App) printNDJSON(value any) {
	encoder := json.NewEncoder(a.Stdout)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		fmt.Fprintf(a.Stderr, "devman: cannot encode output: %v\n", err)
	}
}

// table is a minimal column renderer.
type table struct {
	headers []string
	rows    [][]string
}

func newTable(headers ...string) *table {
	return &table{headers: headers}
}

func (t *table) add(cells ...string) {
	row := make([]string, len(t.headers))
	for i := range row {
		if i < len(cells) {
			row[i] = cells[i]
		}
	}
	t.rows = append(t.rows, row)
}

func (t *table) render(w io.Writer) {
	if len(t.rows) == 0 {
		return
	}
	// Empty trailing columns are dropped so a table without URLs does not carry
	// a ragged empty column.
	keep := make([]bool, len(t.headers))
	for i := range t.headers {
		for _, row := range t.rows {
			if strings.TrimSpace(row[i]) != "" && row[i] != "-" {
				keep[i] = true
				break
			}
		}
	}
	// The first column is always kept, whatever it contains.
	keep[0] = true

	writer := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, joinKept(t.headers, keep))
	for _, row := range t.rows {
		fmt.Fprintln(writer, joinKept(row, keep))
	}
	_ = writer.Flush()
}

func joinKept(cells []string, keep []bool) string {
	parts := make([]string, 0, len(cells))
	for i, cell := range cells {
		if !keep[i] {
			continue
		}
		if cell == "" {
			cell = "-"
		}
		parts = append(parts, cell)
	}
	return strings.Join(parts, "\t")
}
