package buildah

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"

	"coopr/internal/definition"
	"coopr/internal/planner"
)

// stageProgress is local to one prepared stage, including nested invocations.
// It describes existing execution; it never creates a checkpoint for display.
type stageProgress struct {
	writer io.Writer
	prefix string
	total  int
}

func (progress stageProgress) line(format string, args ...any) {
	if progress.writer == nil {
		return
	}
	_, _ = fmt.Fprintln(progress.writer, progress.prefix+fmt.Sprintf(format, args...))
}

func progressText(value string) string {
	first, _, multiline := strings.Cut(value, "\n")
	first = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, first)
	if multiline {
		first = strings.TrimSpace(first) + " ..."
	}
	return first
}

func (progress stageProgress) step(number int, instruction definition.Instruction) {
	text := strings.ToUpper(instruction.Name)
	// Convert normalized command forms back to their authored display syntax.
	// Work on value copies so history, execution, and cache inputs stay unchanged.
	switch instruction.Name {
	case "shell":
		instruction.Form = "exec"
	case "run":
		if len(instruction.InlineFiles) == 1 {
			instruction.Form = "shell"
			instruction.Arguments = []string{instruction.InlineFiles[0].Data}
		}
	}
	arguments := historyInstructionArguments(instruction)
	if instruction.Name == "healthcheck" && len(instruction.Arguments) > 1 {
		command := instruction
		command.Properties = nil
		command.Arguments = instruction.Arguments[1:]
		if instruction.Arguments[0] == "CMD" {
			command.Form = "exec"
		} else {
			command.Form = "shell"
		}
		flags := instruction
		flags.Arguments = nil
		flags.Form = ""
		arguments = historyInstructionArguments(flags)
		if arguments != "" {
			arguments += " "
		}
		arguments += "CMD " + historyInstructionArguments(command)
	}
	if arguments != "" {
		text += " " + arguments
	}
	progress.line("STEP %d/%d: %s", number, progress.total, progressText(text))
}

func (progress stageProgress) cache(imageID string) {
	if imageID != "" {
		progress.line("--> Using cache %s", imageID)
	}
}

func (progress stageProgress) image(imageID string) {
	if imageID == "" {
		return
	}
	if len(imageID) > 12 {
		imageID = imageID[:12]
	}
	progress.line("--> %s", imageID)
}

func (progress stageProgress) commit(reference string) {
	if reference == "" {
		progress.line("COMMIT")
	} else {
		progress.line("COMMIT %s", progressText(reference))
	}
}

// Authored definitions keep the denominator stable when replanning activates
// additional dependencies, including forward stages from inherited ONBUILD.
func definitionStageCount(def *definition.Definition) int {
	if def == nil {
		return 0
	}
	total := 0
	for _, instruction := range def.Instructions {
		switch instruction.Name {
		case "from", "package", "extend":
			total++
		}
	}
	return total
}

func graphProgressPrefix(stage planner.Stage, parent string, authoredTotal int) string {
	ordinal, err := strconv.Atoi(stage.ID)
	if err == nil && ordinal >= 0 && ordinal < authoredTotal {
		if authoredTotal > 1 {
			return parent + fmt.Sprintf("[%d/%d] ", ordinal+1, authoredTotal)
		}
		return parent
	}
	// A selected graph can gain stages during replanning. Without its authored
	// definition, a stable stage identity is more honest than a guessed total.
	label := stage.Name
	if label == "" {
		label = stage.ID
	}
	return parent + "[stage " + progressText(label) + "] "
}
