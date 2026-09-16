package main

import (
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// orderedFlagUsages renders a flag set with flags that have a one-letter
// shorthand first, then long-only flags, each group alphabetical. Cobra's
// default is alphabetical by long name alone, which puts -h, --help in the
// middle of the long-only flags.
func orderedFlagUsages(fs *pflag.FlagSet) string {
	var short, long []*pflag.Flag
	fs.VisitAll(func(f *pflag.Flag) {
		if f.Shorthand != "" {
			short = append(short, f)
		} else {
			long = append(long, f)
		}
	})
	byName := func(flags []*pflag.Flag) {
		sort.Slice(flags, func(i, j int) bool { return flags[i].Name < flags[j].Name })
	}
	byName(short)
	byName(long)

	ordered := pflag.NewFlagSet(fs.Name(), pflag.ContinueOnError)
	ordered.SortFlags = false
	for _, f := range append(short, long...) {
		ordered.AddFlag(f)
	}
	return ordered.FlagUsages()
}

// useShorthandFirstFlagOrder makes every command under root list its flags
// with orderedFlagUsages. Subcommands inherit the root's usage template.
func useShorthandFirstFlagOrder(root *cobra.Command) {
	cobra.AddTemplateFunc("orderedFlagUsages", orderedFlagUsages)
	tmpl := root.UsageTemplate()
	for _, set := range []string{"LocalFlags", "InheritedFlags"} {
		old := "{{." + set + ".FlagUsages"
		if !strings.Contains(tmpl, old) {
			// A cobra upgrade changed its template; keep its default order
			// rather than render a broken help screen.
			return
		}
		tmpl = strings.ReplaceAll(tmpl, old, "{{orderedFlagUsages ."+set)
	}
	root.SetUsageTemplate(tmpl)
}
