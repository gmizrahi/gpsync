package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// completionCmd overrides cobra's built-in completion generator (which
// would otherwise auto-add itself, since we're claiming the "completion"
// name -- see cobra's InitDefaultCompletionCmd) purely to prepend a
// commented usage header to the generated script. `gpsync completion
// powershell --help` already explains how to load it, but running the
// command directly -- the way it's actually meant to be used, piped
// straight into a shell -- printed nothing but the raw script with no clue
// how to use it unless you already knew. The header lines are `#`-prefixed
// comments in every one of these shells, so they're inert when piped into
// Invoke-Expression/eval/source -- this is purely additive, safe to leave
// in the piped output every time, not just discoverable via --help.
func completionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "completion [bash|zsh|fish|powershell]",
		Short: "Generate the autocompletion script for the specified shell",
	}
	cmd.AddCommand(
		completionShellCmd("bash", "One-off (current session only):\n  source <(gpsync completion bash)\nPermanent: add that line to ~/.bashrc.",
			func(w io.Writer, root *cobra.Command) error { return root.GenBashCompletionV2(w, true) }),
		completionShellCmd("zsh", "One-off (current session only):\n  source <(gpsync completion zsh)\nPermanent: save the output somewhere on your $fpath, e.g. \"gpsync completion zsh > \"${fpath[1]}/_gpb\"\".",
			func(w io.Writer, root *cobra.Command) error { return root.GenZshCompletion(w) }),
		completionShellCmd("fish", "One-off (current session only):\n  gpsync completion fish | source\nPermanent: gpsync completion fish > ~/.config/fish/completions/gpsync.fish",
			func(w io.Writer, root *cobra.Command) error { return root.GenFishCompletion(w, true) }),
		completionShellCmd("powershell", "One-off (current session only):\n  .\\gpsync.exe completion powershell | Out-String | Invoke-Expression\nPermanent: add that same line to your PowerShell profile ($PROFILE).\nAlso sets a \"gpsync\" alias to this exe's full path, so you can type \"gpsync\" instead of \".\\gpsync.exe\" or the full path.",
			genPowerShellCompletionWithAlias),
	)
	return cmd
}

// genPowerShellCompletionWithAlias emits a `Set-Alias gpsync <this exe's own
// absolute path>` before the completion script, so a fresh PowerShell
// session gets a plain "gpsync" command instead of needing ".\gpsync.exe" or the
// full path -- PowerShell (unlike cmd.exe) doesn't search the current
// directory by default, which is exactly the friction this removes.
// os.Executable() is asked at generation time, i.e. it resolves to
// wherever THIS running exe actually lives (e.g. C:\GPSync\gpsync.exe)
// -- not hardcoded, so it keeps working if the exe is ever moved/renamed,
// as long as the completion script is regenerated from the new location.
// No quote-escaping needed: Windows paths can never contain a literal `"`.
func genPowerShellCompletionWithAlias(w io.Writer, root *cobra.Command) error {
	if exe, err := os.Executable(); err == nil {
		fmt.Fprintf(w, "Set-Alias -Name gpsync -Value \"%s\"\n", exe)
	}
	return root.GenPowerShellCompletionWithDesc(w)
}

func completionShellCmd(shell, usage string, gen func(w io.Writer, root *cobra.Command) error) *cobra.Command {
	return &cobra.Command{
		Use:   shell,
		Short: fmt.Sprintf("Generate the autocompletion script for %s", shell),
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			// Printed AFTER the script, not before: the script itself can be
			// hundreds of lines, and a header at the top just scrolls out of
			// view -- putting it at the bottom means it's what's actually on
			// screen (and easiest to copy-paste) right when the command
			// finishes.
			if err := gen(w, cmd.Root()); err != nil {
				return err
			}
			fmt.Fprintln(w)
			for _, line := range strings.Split(usage, "\n") {
				fmt.Fprintf(w, "# %s\n", line)
			}
			return nil
		},
	}
}

// ── setup ───────────────────────────────────────────────────────────────
