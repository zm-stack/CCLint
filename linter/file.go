package linter

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	"math"
	"regexp"
	"strings"
)

// File abstraction used for representing files.
type File struct {
	Name    string
	Pkg     *Package
	content []byte
	AST     *ast.File
}

// NewFile creates a new file
func NewFile(name string, content []byte, pkg *Package) (*File, error) {
	f, err := parser.ParseFile(pkg.fset, name, content, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	return &File{
		Name:    name,
		content: content,
		Pkg:     pkg,
		AST:     f,
	}, nil
}

// ToPosition returns line and column for given position.
func (f *File) ToPosition(pos token.Pos) token.Position {
	return f.Pkg.fset.Position(pos)
}

// Render renters a node.
func (f *File) Render(x interface{}) string {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, f.Pkg.fset, x); err != nil {
		panic(err)
	}
	return buf.String()
}

var basicTypeKinds = map[types.BasicKind]string{
	types.UntypedBool:    "bool",
	types.UntypedInt:     "int",
	types.UntypedRune:    "rune",
	types.UntypedFloat:   "float64",
	types.UntypedComplex: "complex128",
	types.UntypedString:  "string",
}

// IsUntypedConst reports whether expr is an untyped constant,
// and indicates what its default type is.
// scope may be nil.
func (f *File) IsUntypedConst(expr ast.Expr) (defType string, ok bool) {
	// Re-evaluate expr outside of its context to see if it's untyped.
	// (An expr evaluated within, for example, an assignment context will get the type of the LHS.)
	exprStr := f.Render(expr)
	tv, err := types.Eval(f.Pkg.fset, f.Pkg.TypesPkg, expr.Pos(), exprStr)
	if err != nil {
		return "", false
	}
	if b, ok := tv.Type.(*types.Basic); ok {
		if dt, ok := basicTypeKinds[b.Kind()]; ok {
			return dt, true
		}
	}

	return "", false
}

func (f *File) isMain() bool {
	if f.AST.Name.Name == "main" {
		return true
	}
	return false
}

func (f *File) lint(rules []Rule, rulesConfig RulesConfig, failures chan Failure) {
	disabledIntervals := f.disabledIntervals(rules)
	for _, currentRule := range rules {
		config := rulesConfig[currentRule.Name()]
		currentFailures := currentRule.Apply(f, config)
		for idx, failure := range currentFailures {
			if failure.RuleName == "" {
				failure.RuleName = currentRule.Name()
			}
			if failure.Node != nil {
				failure.Position = ToFailurePosition(failure.Node.Pos(), failure.Node.End(), f)
			}
			currentFailures[idx] = failure
		}
		currentFailures = f.filterFailures(currentFailures, disabledIntervals)
		for _, failure := range currentFailures {
			failures <- failure
		}
	}
}

type enableDisableConfig struct {
	enabled  bool
	position int
}

func (f *File) disabledIntervals(rules []Rule) disabledIntervalsMap {
	re := regexp.MustCompile(`^\s*revive:(enable|disable)(?:-(line|next-line))?(:|\s|$)`)

	enabledDisabledRulesMap := make(map[string][]enableDisableConfig)

	getEnabledDisabledIntervals := func() disabledIntervalsMap {
		result := make(disabledIntervalsMap)

		for ruleName, disabledArr := range enabledDisabledRulesMap {
			ruleResult := []DisabledInterval{}
			for i := 0; i < len(disabledArr); i++ {
				interval := DisabledInterval{
					RuleName: ruleName,
					From: token.Position{
						Filename: f.Name,
						Line:     disabledArr[i].position,
					},
					To: token.Position{
						Filename: f.Name,
						Line:     math.MaxInt32,
					},
				}
				if i%2 == 0 {
					ruleResult = append(ruleResult, interval)
				} else {
					ruleResult[len(ruleResult)-1].To.Line = disabledArr[i].position
				}
			}
			result[ruleName] = ruleResult
		}

		return result
	}

	handleConfig := func(isEnabled bool, line int, name string) {
		existing, ok := enabledDisabledRulesMap[name]
		if !ok {
			existing = []enableDisableConfig{}
			enabledDisabledRulesMap[name] = existing
		}
		if (len(existing) > 1 && existing[len(existing)-1].enabled == isEnabled) ||
			(len(existing) == 0 && isEnabled) {
			return
		}
		existing = append(existing, enableDisableConfig{
			enabled:  isEnabled,
			position: line,
		})
		enabledDisabledRulesMap[name] = existing
	}

	handleRules := func(filename, modifier string, isEnabled bool, line int, ruleNames []string) []DisabledInterval {
		var result []DisabledInterval
		for _, name := range ruleNames {
			if modifier == "line" {
				handleConfig(isEnabled, line, name)
				handleConfig(!isEnabled, line, name)
			} else if modifier == "next-line" {
				handleConfig(isEnabled, line+1, name)
				handleConfig(!isEnabled, line+1, name)
			} else {
				handleConfig(isEnabled, line, name)
			}
		}
		return result
	}

	handleComment := func(filename string, c *ast.CommentGroup, line int) {
		text := c.Text()
		parts := re.FindStringSubmatch(text)
		if len(parts) == 0 {
			return
		}
		str := re.FindString(text)
		ruleNamesString := strings.Split(text, str)
		ruleNames := []string{}
		if len(ruleNamesString) == 2 {
			tempNames := strings.Split(ruleNamesString[1], ",")
			for _, name := range tempNames {
				name = strings.Trim(name, "\n")
				if len(name) > 0 {
					ruleNames = append(ruleNames, name)
				}
			}
		}

		// TODO: optimize
		if len(ruleNames) == 0 {
			for _, rule := range rules {
				ruleNames = append(ruleNames, rule.Name())
			}
		}

		handleRules(filename, parts[2], parts[1] == "enable", line, ruleNames)
	}

	comments := f.AST.Comments
	for _, c := range comments {
		handleComment(f.Name, c, f.ToPosition(c.Pos()).Line)
	}

	return getEnabledDisabledIntervals()
}

func (f *File) filterFailures(failures []Failure, disabledIntervals disabledIntervalsMap) []Failure {
	result := []Failure{}
	for _, failure := range failures {
		fStart := failure.Position.Start.Line
		fEnd := failure.Position.End.Line
		intervals, ok := disabledIntervals[failure.RuleName]
		if !ok {
			result = append(result, failure)
		} else {
			include := true
			for _, interval := range intervals {
				intStart := interval.From.Line
				intEnd := interval.To.Line
				if (fStart >= intStart && fStart <= intEnd) ||
					(fEnd >= intStart && fEnd <= intEnd) {
					include = false
					break
				}
			}
			if include {
				result = append(result, failure)
			}
		}
	}
	return result
}