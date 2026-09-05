package main

import (
	"fmt"
	"os"
)

// Tab completion treats the tool slot like the shell treats the start of a
// command line: `rein ph<tab>` should offer php the way `sudo ph<tab>` does.
// Each script completes rein's own verbs and PATH executables in position one,
// PATH executables after in/run/spec, and shell names after completion.

const bashCompletion = `# bash completion for rein — add to ~/.bashrc:
#   eval "$(rein completion bash)"
_rein() {
    local cur=${COMP_WORDS[COMP_CWORD]}
    if [[ $COMP_CWORD -eq 1 ]]; then
        COMPREPLY=( $(compgen -W "in spec list mcp configure gateway update version help completion" -- "$cur")
                    $(compgen -c -- "$cur") )
        return
    fi
    case ${COMP_WORDS[1]} in
        configure)
            COMPREPLY=( $(compgen -W "claude-code codex --scope --persistent --gateway --backend --model --dry-run --apply --register --check --launch --undo" -- "$cur") ) ;;
        gateway)
            COMPREPLY=( $(compgen -W "start serve status stop connect" -- "$cur") ) ;;
        in|run|spec)
            [[ $COMP_CWORD -eq 2 ]] && COMPREPLY=( $(compgen -c -- "$cur") ) ;;
        completion)
            [[ $COMP_CWORD -eq 2 ]] && COMPREPLY=( $(compgen -W "bash zsh fish" -- "$cur") ) ;;
    esac
}
complete -F _rein rein
`

const zshCompletion = `#compdef rein
# zsh completion for rein — add to ~/.zshrc:
#   eval "$(rein completion zsh)"
_rein() {
    if (( CURRENT == 2 )); then
        _alternative \
            'verbs:rein command:(in spec list mcp configure gateway update version help completion)' \
            'tools:tool:_command_names -e'
        return
    fi
    case $words[2] in
        configure)
            _values configure claude-code codex --scope --persistent --gateway --backend --model --dry-run --apply --register --check --launch --undo ;;
        gateway)
            _values gateway start serve status stop connect ;;
        in|run|spec)
            (( CURRENT == 3 )) && _command_names -e ;;
        completion)
            (( CURRENT == 3 )) && _values shell bash zsh fish ;;
    esac
}
if [ "$funcstack[1]" = "_rein" ]; then
    _rein "$@"
else
    compdef _rein rein
fi
`

const fishCompletion = `# fish completion for rein — save as ~/.config/fish/completions/rein.fish, or:
#   rein completion fish | source
complete -c rein -f
complete -c rein -n "__fish_use_subcommand" -a "in spec list mcp configure gateway update version help completion" -d "rein command"
complete -c rein -n "__fish_seen_subcommand_from configure" -a "claude-code codex --scope --persistent --gateway --backend --model --dry-run --apply --register --check --launch --undo"
complete -c rein -n "__fish_seen_subcommand_from gateway" -a "start serve status stop connect"
complete -c rein -n "__fish_use_subcommand" -a "(__fish_complete_command)"
complete -c rein -n "__fish_seen_subcommand_from in run spec; and test (count (commandline -opc)) -eq 2" -a "(__fish_complete_command)"
complete -c rein -n "__fish_seen_subcommand_from completion" -a "bash zsh fish"
`

func cmdCompletion(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: rein completion <bash|zsh|fish>")
	}
	switch args[0] {
	case "bash":
		fmt.Print(bashCompletion)
	case "zsh":
		fmt.Print(zshCompletion)
	case "fish":
		fmt.Print(fishCompletion)
	default:
		return fmt.Errorf("unknown shell %q: want bash, zsh, or fish", args[0])
	}
	_ = os.Stdout.Sync()
	return nil
}
