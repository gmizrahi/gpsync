package main

import "strings"

// repairWindowsArgs fixes paths that end in a backslash when gpsync is run
// from Windows PowerShell 5.1. Tab completion appends a trailing
// backslash, and `gpsync upload 'C:\Photos\2023\2023_04 Trip\'` then
// reported "Nothing pending" while the same path without the backslash
// worked.
//
// The cause is Windows argument parsing, not path handling. PowerShell 5.1
// quotes an argument containing a space when it launches an exe, producing
// "C:\Photos\2026\2026_09_at_the_park\" on the raw command line. The Microsoft C
// runtime rules Go follows read \" as an ESCAPED quote, so the closing
// quote becomes a literal character and quoting never ends. Verified on
// Windows with a probe exe: one such path arrives as
// `C:\...\2023_04 Trip"`, and with more paths after it the arguments
// merge and split in the wrong places:
//
//	arg[0] = [C:\Photos\2023\2023_04 Trip" C:\NoSpace\ C:\Photos\2023\2023_04]
//	arg[1] = [Trip]
//
// So stripping a stray quote would not be a fix. What makes a real repair
// possible is that `"` cannot appear in a Windows file or folder name at
// all: any parsed argument containing one is an artifact. In that case the
// raw command line is re-split with backslashes treated as ordinary
// characters, which is what someone typing a Windows path means.
//
// raw is the full command line including the program name (from
// GetCommandLineW). It returns the repaired arguments and true, or args
// unchanged and false when nothing needed repairing or raw is unavailable.
func repairWindowsArgs(args []string, raw string) ([]string, bool) {
	if raw == "" {
		return args, false
	}
	broken := false
	for _, a := range args {
		if strings.Contains(a, `"`) {
			broken = true
			break
		}
	}
	if !broken {
		return args, false
	}
	tokens := splitCommandLineLiteralBackslashes(raw)
	if len(tokens) == 0 {
		return args, false
	}
	return tokens[1:], true // drop the program name
}

// splitCommandLineLiteralBackslashes splits a Windows command line on
// whitespace outside double quotes. A double quote toggles quoting and is
// never part of the result; a backslash is always a literal character.
// That last rule is the whole difference from the C runtime's parser, and
// it is correct for gpsync because none of its arguments can legitimately
// contain a double quote.
func splitCommandLineLiteralBackslashes(raw string) []string {
	var out []string
	var cur strings.Builder
	inQuotes, inArg := false, false
	for _, r := range raw {
		switch {
		case r == '"':
			inQuotes = !inQuotes
			inArg = true // "" is a real, empty argument
		case (r == ' ' || r == '\t') && !inQuotes:
			if inArg {
				out = append(out, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteRune(r)
			inArg = true
		}
	}
	if inArg {
		out = append(out, cur.String())
	}
	return out
}
