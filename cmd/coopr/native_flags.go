package main

import (
	"fmt"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	commoncompletion "go.podman.io/common/pkg/completion"
)

// Import only the native options supported by this command. Native values remain
// bound to Buildah's result structs; no second parser or synchronization is needed.
func addNativeFlags(command *cobra.Command, source *pflag.FlagSet, completions commoncompletion.FlagCompletions, names ...string) {
	for _, name := range names {
		flag := source.Lookup(name)
		if flag == nil {
			panic(fmt.Sprintf("native Buildah option %q is unavailable", name))
		}
		command.Flags().AddFlag(flag)
		if completion := completions[name]; completion != nil {
			_ = command.RegisterFlagCompletionFunc(name, completion)
		}
	}
}

func setNativeDefault(flag *pflag.Flag, value string) {
	if flag == nil {
		return
	}
	if err := flag.Value.Set(value); err != nil {
		panic(err)
	}
	flag.DefValue = value
}
