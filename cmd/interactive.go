package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"awsmux/internal/core"
)

// isTTY reports whether stdin is an interactive terminal.
func isTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// pickTargets runs a numbered checkbox picker over targets. Everything
// starts selected; each input line toggles ("3", "1-4", comma or space
// separated), "a" selects all, "n" selects none, and an empty line accepts
// the current selection.
func pickTargets(in io.Reader, out io.Writer, targets []core.Target) ([]core.Target, error) {
	if len(targets) == 0 {
		return nil, errors.New("no targets to pick from")
	}
	selected := make([]bool, len(targets))
	for i := range selected {
		selected[i] = true
	}
	scanner := bufio.NewScanner(in)
	for {
		pickPrintList(out, targets, selected)
		fmt.Fprint(out, "> ")
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return nil, fmt.Errorf("reading selection: %w", err)
			}
			return nil, errors.New("input closed before the selection was accepted")
		}
		line := strings.TrimSpace(scanner.Text())
		switch line {
		case "":
			var picked []core.Target
			for i, t := range targets {
				if selected[i] {
					picked = append(picked, t)
				}
			}
			if len(picked) == 0 {
				fmt.Fprintln(out, "nothing selected; toggle at least one target (or ctrl-c to abort)")
				continue
			}
			return picked, nil
		case "a":
			for i := range selected {
				selected[i] = true
			}
		case "n":
			for i := range selected {
				selected[i] = false
			}
		default:
			if err := pickToggle(selected, line); err != nil {
				fmt.Fprintln(out, err.Error())
			}
		}
	}
}

// confirmMutation enforces the interactive confirmation rules: read only
// runs freely, mutating (and unknown) requires typing "yes", destructive
// requires typing the operation name exactly.
func confirmMutation(in io.Reader, out io.Writer, classification core.Classification, operation string) error {
	var want string
	switch classification {
	case core.ClassReadOnly:
		return nil
	case core.ClassDestructive:
		want = operation
		fmt.Fprintf(out, "This operation is DESTRUCTIVE. Type the operation name (%s) to continue: ", operation)
	default:
		want = "yes"
		fmt.Fprintf(out, "This operation is %s. Type \"yes\" to continue: ", strings.ToUpper(string(classification)))
	}
	scanner := bufio.NewScanner(in)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("reading confirmation: %w", err)
		}
		return errors.New("input closed before confirmation")
	}
	if got := strings.TrimSpace(scanner.Text()); got != want {
		return fmt.Errorf("confirmation %q did not match %q, aborted", got, want)
	}
	return nil
}

func pickPrintList(out io.Writer, targets []core.Target, selected []bool) {
	fmt.Fprintln(out, "Select targets (numbers or ranges toggle, a = all, n = none, enter = accept):")
	for i, t := range targets {
		mark := " "
		if selected[i] {
			mark = "x"
		}
		desc := t.ID
		if t.AccountID != "" {
			desc += "  " + t.AccountID
		}
		fmt.Fprintf(out, "  [%s] %2d  %s\n", mark, i+1, desc)
	}
}

// pickToggle applies one input line of toggles ("3", "1-4", comma or space
// separated) to the selection.
func pickToggle(selected []bool, line string) error {
	tokens := strings.FieldsFunc(line, func(r rune) bool {
		return r == ' ' || r == '\t' || r == ','
	})
	for _, tok := range tokens {
		lo, hi, err := pickParseRange(tok)
		if err != nil {
			return err
		}
		if lo < 1 || hi > len(selected) || lo > hi {
			return fmt.Errorf("selection %q is out of range 1-%d", tok, len(selected))
		}
		for i := lo; i <= hi; i++ {
			selected[i-1] = !selected[i-1]
		}
	}
	return nil
}

func pickParseRange(tok string) (lo, hi int, err error) {
	if a, b, found := strings.Cut(tok, "-"); found {
		lo, err = strconv.Atoi(a)
		if err == nil {
			hi, err = strconv.Atoi(b)
		}
	} else {
		lo, err = strconv.Atoi(tok)
		hi = lo
	}
	if err != nil {
		return 0, 0, fmt.Errorf("cannot parse selection %q (use a number, a range like 2-5, a, or n)", tok)
	}
	return lo, hi, nil
}
