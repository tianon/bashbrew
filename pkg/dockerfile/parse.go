package dockerfile

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"
)

type Stage struct {
	From      string // image name (or parent stage's image name, if "FROM stage-name")
	FromStage string // original stage name, if "FROM stage-name"
	Name      string // empty for unnamed stages
	Platform  string // empty string, $BUILDPLATFORM, or $TARGETPLATFORM
	// TODO somehow, we need to expose the platform of each stage to meta-scripts so it can know that, for example, the build stage base image is only needed for the *host* platform, not the target platform
}

type Metadata struct {
	Stages      []Stage
	NamedStages map[string]int // map of stage names to index in Stages slice

	Froms []string // every "FROM" or "COPY --from=xxx" value (minus named and/or numbered stages in the case of "--from=")
}

func Parse(dockerfile string) (Metadata, error) {
	return ParseReader(strings.NewReader(dockerfile))
}

func ParseReader(dockerfile io.Reader) (Metadata, error) {
	meta := Metadata{
		// panic: assignment to entry in nil map
		NamedStages: map[string]int{},
		// (nil slices work fine)
	}

	scanner := bufio.NewScanner(dockerfile)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if line == "" {
			// ignore straight up blank lines (no complexity)
			continue
		}

		// (we can't have a comment that ends in a continuation line - that's not continuation, that's part of the comment)
		if line[0] == '#' {
			// TODO handle "escape" parser directive
			// TODO handle "syntax" parser directive -- explode appropriately (since custom syntax invalidates our Dockerfile parsing)
			// ignore comments
			continue
		}

		// handle line continuations
		// (TODO see note above regarding "escape" parser directive)
		for line[len(line)-1] == '\\' {
			if !scanner.Scan() {
				line = line[0 : len(line)-1]
				break
			}
			// "strings.TrimRightFunc(IsSpace)" because whitespace *after* the escape character is supported and ignored 🙈
			nextLine := strings.TrimRightFunc(scanner.Text(), unicode.IsSpace)
			if nextLine == "" { // if it's all space, TrimRight will be TrimSpace 😏
				// ignore "empty continuation" lines (https://github.com/moby/moby/pull/33719)
				continue
			}
			if strings.TrimLeftFunc(nextLine, unicode.IsSpace)[0] == '#' {
				// ignore comments inside continuation (https://github.com/moby/moby/issues/29005)
				continue
			}
			line = line[0:len(line)-1] + nextLine
		}

		// TODO *technically* a line like "   RUN     echo  hi    " should be parsed as "RUN" "echo  hi" (cut off instruction, then the rest of the line with TrimSpace), but for our needs "strings.Fields" is good enough for now

		// line = strings.TrimSpace(line) // (emulated below; "strings.Fields" does essentially the same exact thing so we don't need to do it explicitly here too)

		fields := strings.Fields(line)

		if len(fields) < 1 {
			// ignore empty lines
			continue
		}

		instruction := strings.ToUpper(fields[0])

		args := fields[1:]

		switch instruction {
		case "FROM":
			var stage Stage
			if platform, ok := strings.CutPrefix(args[0], "--platform="); ok {
				stage.Platform = platform
				args = args[1:]
				switch stage.Platform {
				case "$BUILDPLATFORM", "$TARGETPLATFORM":
					// explicitly allowed for more efficient cross-compiling (see also condition outside the meta loop to ensure the final stage is either without platform or explicitly --platform=$TARGETPLATFORM)
				default:
					return meta, fmt.Errorf("FROM has unsupported --platform=%q -- any --platform must be generic or unspecified for correct dependency calculation", stage.Platform)
				}
			}

			stage.From = args[0]
			args = args[1:]

			if strings.ContainsRune(stage.From, '$') {
				return meta, fmt.Errorf("FROM %q contains invalid/disallowed character '$' -- explicit FROM values are required for dependency calculation", stage.From)
			}

			if i, ok := meta.NamedStages[stage.From]; ok {
				// if this is a valid stage name, we should resolve it back to the original FROM value of that previous stage (we don't care about inter-stage dependencies for the purposes of either tag dependency calculation or tag building -- just how many there are and what external things they require)
				parent := meta.Stages[i]
				if stage.Platform == "" {
					stage.Platform = parent.Platform
				} else if stage.Platform != parent.Platform {
					return meta, fmt.Errorf("FROM %q has --platform=%q but stage %q has --platform=%q", stage.From, stage.Platform, stage.From, parent.Platform)
				}
				stage.FromStage = stage.From
				stage.From = parent.From
			} else {
				// make sure to add ":latest" if it's implied
				stage.From = latestizeRepoTag(stage.From)
			}

			i := len(meta.Stages)
			if len(args) == 2 && strings.ToUpper(args[0]) == "AS" {
				stage.Name = args[1]
				meta.NamedStages[stage.Name] = i
			}
			meta.Stages = append(meta.Stages, stage)

			meta.Froms = append(meta.Froms, stage.From)

		case "COPY":
			for _, arg := range args {
				if !strings.HasPrefix(arg, "--") {
					// doesn't appear to be a "flag"; time to bail!
					break
				}
				from, ok := strings.CutPrefix(arg, "--from=")
				if !ok {
					// ignore any flags we're not interested in
					continue
				}

				if i, ok := meta.NamedStages[from]; ok {
					// see note above regarding stage names in FROM
					from = meta.Stages[i].From
				} else if stageNumber, err := strconv.Atoi(from); err == nil && stageNumber < len(meta.Stages) {
					// must be a stage number, we should resolve it too
					from = meta.Stages[stageNumber].From
				} else {
					// make sure to add ":latest" if it's implied
					from = latestizeRepoTag(from)
				}

				meta.Froms = append(meta.Froms, from)
			}

		case "RUN": // TODO combine this and the above COPY-parsing code somehow sanely
			for _, arg := range args {
				if !strings.HasPrefix(arg, "--") {
					// doesn't appear to be a "flag"; time to bail!
					break
				}
				csv, ok := strings.CutPrefix(arg, "--mount=")
				if !ok {
					// ignore any flags we're not interested in
					continue
				}
				// TODO more correct CSV parsing
				fields := strings.Split(csv, ",")
				var mountType, from string
				for _, field := range fields {
					if val, ok := strings.CutPrefix(field, "type="); ok {
						mountType = val
						continue
					}
					if val, ok := strings.CutPrefix(field, "from="); ok {
						from = val
						continue
					}
				}
				if mountType != "bind" || from == "" {
					// this is probably something we should be worried about, but not something we're interested in parsing
					continue
				}

				if i, ok := meta.NamedStages[from]; ok {
					// see note above regarding stage names in FROM
					from = meta.Stages[i].From
				} else if stageNumber, err := strconv.Atoi(from); err == nil && stageNumber < len(meta.Stages) {
					// must be a stage number, we should resolve it too
					from = meta.Stages[stageNumber].From
				} else {
					// make sure to add ":latest" if it's implied
					from = latestizeRepoTag(from)
				}

				meta.Froms = append(meta.Froms, from)
			}
		}
	}

	// TODO maybe we *shouldn't* support parsing a fully empty Dockerfile? 🤔 (we actively use an "empty" Dockerfile in the tests to test edge cases of continuation though that are otherwise hard to test, so it's probably ~fine)
	if len(meta.Stages) > 0 {
		finalStage := meta.Stages[len(meta.Stages)-1]
		switch finalStage.Platform {
		case "", "$TARGETPLATFORM":
			// yay, all is well
		default:
			return meta, fmt.Errorf("final stage/FROM (%q) has --platform=%q but must be unspecified or $TARGETPLATFORM", finalStage.From, finalStage.Platform)
		}
	}

	return meta, scanner.Err()
}

func latestizeRepoTag(repoTag string) string {
	if repoTag != "scratch" && strings.IndexRune(repoTag, ':') < 0 {
		return repoTag + ":latest"
	}
	return repoTag
}
