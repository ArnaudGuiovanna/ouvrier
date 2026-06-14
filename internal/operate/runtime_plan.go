package operate

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

func (r *AgentRuntime) planPrompt(text string) promptPlan {
	if strings.HasPrefix(strings.TrimSpace(text), "/") {
		return r.planSlash(text)
	}
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "login") && strings.Contains(lower, "codex"):
		return promptPlan{Tools: []plannedTool{{Name: "login_codex"}}}
	case looksLikeCreateWorker(lower):
		return r.planCreateWorker(text)
	case strings.Contains(lower, "review"):
		return promptPlan{Tools: []plannedTool{{Name: "read_ouvrier_api"}, {Name: "review_worker", Input: map[string]any{"subject": text}}}}
	case strings.Contains(lower, "fix") || strings.Contains(lower, "repair") || strings.Contains(lower, "corrige"):
		return promptPlan{Tools: []plannedTool{{Name: "fix_worker", Input: map[string]any{"subject": text}}}}
	case strings.Contains(lower, "audit"):
		return promptPlan{Tools: []plannedTool{{Name: "audit_worker"}}}
	case strings.Contains(lower, "diff"):
		return promptPlan{Tools: []plannedTool{{Name: "diff_worker"}}}
	case strings.Contains(lower, "deploy") || strings.Contains(lower, "transfer") || strings.Contains(lower, "déplo"):
		env := inferDeployEnv(text)
		return promptPlan{Tools: []plannedTool{{Name: "build_worker"}, {Name: "transfer_worker", Input: map[string]any{"env": env}}}}
	case strings.Contains(lower, "build") || strings.Contains(lower, "binary") || strings.Contains(lower, "binaire"):
		return promptPlan{Tools: []plannedTool{{Name: "build_worker", Input: map[string]any{"target": inferTarget(text)}}}}
	default:
		if r.workspace == nil {
			return promptPlan{Assistant: "No worker is selected yet. Describe a worker to create, for example: create a worker that receives POST /tickets, or use /new worker --name ticket-triage --trigger \"POST /tickets\"."}
		}
		return promptPlan{Tools: []plannedTool{{Name: "read_ouvrier_api"}, {Name: "patch_worker", Input: map[string]any{"goal": text}}}}
	}
}

func (r *AgentRuntime) planSlash(text string) promptPlan {
	args, err := splitArgs(strings.TrimSpace(strings.TrimPrefix(text, "/")))
	if err != nil {
		return promptPlan{Assistant: "I could not parse that command: " + err.Error()}
	}
	if len(args) == 0 {
		return promptPlan{Assistant: operateSlashHelp()}
	}
	cmd := strings.ToLower(args[0])
	rest := args[1:]
	if cmd == "new" && len(rest) > 0 && strings.EqualFold(rest[0], "worker") {
		cmd = "new-worker"
		rest = rest[1:]
	}
	switch cmd {
	case "help", "?":
		return promptPlan{Assistant: operateSlashHelp()}
	case "tools":
		names := r.Tools.Names()
		return promptPlan{Assistant: "Available Ouvrier tools:\n- " + strings.Join(names, "\n- ")}
	case "policy":
		return promptPlan{Assistant: "Policy: Ouvrier-native tools are the only path for scaffold, review, audit, build, and transfer. Codex edits run through the configured Driver, session artifacts are JSONL/JSON, and Ouvrier stores no Codex subscription tokens."}
	case "login":
		if len(rest) == 0 || strings.EqualFold(rest[0], "codex") {
			return promptPlan{Tools: []plannedTool{{Name: "login_codex"}}}
		}
		return promptPlan{Assistant: "Only /login codex is implemented in v0.5."}
	case "new-worker", "create-worker":
		opts := parseCommandOptions(rest)
		spec := workerSpecFromOptions(opts, strings.Join(opts.positionals, " "))
		if spec.Name == "" && len(opts.positionals) > 0 {
			spec.Name = opts.positionals[0]
		}
		return r.planWorkerSpec(spec, strings.Join(rest, " "))
	case "review":
		return promptPlan{Tools: []plannedTool{{Name: "read_ouvrier_api"}, {Name: "review_worker", Input: map[string]any{"subject": strings.Join(rest, " ")}}}}
	case "fix":
		return promptPlan{Tools: []plannedTool{{Name: "fix_worker", Input: map[string]any{"subject": strings.Join(rest, " ")}}}}
	case "audit":
		return promptPlan{Tools: []plannedTool{{Name: "audit_worker"}}}
	case "build":
		return promptPlan{Tools: []plannedTool{{Name: "build_worker", Input: map[string]any{"target": firstNonEmpty(parseCommandOptions(rest).values["target"], inferTarget(strings.Join(rest, " ")))}}}}
	case "deploy", "transfer":
		opts := parseCommandOptions(rest)
		env := opts.values["env"]
		if env == "" && len(opts.positionals) > 0 {
			env = opts.positionals[0]
		}
		return promptPlan{Tools: []plannedTool{{Name: "build_worker", Input: map[string]any{"target": opts.values["target"]}}, {Name: "transfer_worker", Input: map[string]any{"env": env, "target": opts.values["target"], "env_file": opts.values["env-file"]}}}}
	case "diff":
		return promptPlan{Tools: []plannedTool{{Name: "diff_worker"}}}
	case "read", "open":
		path := "main.go"
		if len(rest) > 0 {
			path = rest[0]
		}
		return promptPlan{Tools: []plannedTool{{Name: "read_worker_file", Input: map[string]any{"path": path}}}}
	case "docs":
		return promptPlan{Tools: []plannedTool{{Name: "search_ouvrier_docs", Input: map[string]any{"query": strings.Join(rest, " ")}}}}
	case "workers":
		return promptPlan{Tools: []plannedTool{{Name: "list_workers"}}}
	case "accept-risk":
		return promptPlan{Tools: []plannedTool{{Name: "accept_risk", Input: map[string]any{"rationale": strings.Join(rest, " ")}}}}
	case "export":
		return promptPlan{Tools: []plannedTool{{Name: "export_session"}}}
	default:
		return promptPlan{Assistant: "Unknown operate command " + cmd + ".\n" + operateSlashHelp()}
	}
}

func (r *AgentRuntime) planCreateWorker(text string) promptPlan {
	spec := inferWorkerSpec(text)
	return r.planWorkerSpec(spec, text)
}

func (r *AgentRuntime) planWorkerSpec(spec workerSpec, goal string) promptPlan {
	var missing []string
	if spec.Name == "" {
		missing = append(missing, "worker name")
	}
	if spec.Trigger == "" {
		missing = append(missing, "trigger, for example POST /tickets")
	}
	if len(missing) > 0 {
		return promptPlan{Assistant: "I can build this worker, but I need: " + strings.Join(missing, ", ") + "."}
	}
	if spec.Model == "" {
		spec.Model = defaultOperateModel
	}
	tools := []plannedTool{
		{Name: "read_ouvrier_api"},
		{Name: "scaffold_worker", Input: map[string]any{"name": spec.Name, "trigger": spec.Trigger, "model": spec.Model}},
	}
	if strings.TrimSpace(goal) != "" {
		tools = append(tools, plannedTool{Name: "patch_worker", Input: map[string]any{"goal": goal}})
	}
	lower := strings.ToLower(goal)
	if strings.Contains(lower, "review") || strings.Contains(lower, "audit") || strings.Contains(lower, "build") || strings.Contains(lower, "deploy") || strings.Contains(lower, "transfer") {
		tools = append(tools, plannedTool{Name: "review_worker", Input: map[string]any{"subject": "post-generation review"}})
	}
	if strings.Contains(lower, "audit") || strings.Contains(lower, "build") || strings.Contains(lower, "deploy") || strings.Contains(lower, "transfer") {
		tools = append(tools, plannedTool{Name: "audit_worker"})
	}
	if strings.Contains(lower, "build") || strings.Contains(lower, "deploy") || strings.Contains(lower, "transfer") {
		tools = append(tools, plannedTool{Name: "build_worker", Input: map[string]any{"target": inferTarget(goal)}})
	}
	if strings.Contains(lower, "deploy") || strings.Contains(lower, "transfer") {
		tools = append(tools, plannedTool{Name: "transfer_worker", Input: map[string]any{"env": inferDeployEnv(goal)}})
	}
	return promptPlan{Assistant: fmt.Sprintf("Plan: scaffold %s (%s), load Ouvrier API context, then let the configured agent specialize the worker.", spec.Name, spec.Trigger), Tools: tools}
}

type workerSpec struct {
	Name    string
	Trigger string
	Model   string
}

type commandOptions struct {
	values      map[string]string
	positionals []string
}

func workerSpecFromOptions(opts commandOptions, fallback string) workerSpec {
	spec := workerSpec{
		Name:    firstNonEmpty(opts.values["name"], opts.values["n"]),
		Trigger: firstNonEmpty(opts.values["trigger"], opts.values["t"]),
		Model:   firstNonEmpty(opts.values["model"], opts.values["m"]),
	}
	if spec.Trigger == "" {
		spec.Trigger = inferTrigger(fallback)
	}
	if spec.Name == "" {
		spec.Name = inferName(fallback, spec.Trigger)
	}
	return spec
}

func inferWorkerSpec(text string) workerSpec {
	trigger := inferTrigger(text)
	return workerSpec{
		Name:    inferName(text, trigger),
		Trigger: trigger,
		Model:   inferModel(text),
	}
}

var httpTriggerRE = regexp.MustCompile(`(?i)\b(GET|POST)\s+(/[A-Za-z0-9_./:-]+)`)

func inferTrigger(text string) string {
	match := httpTriggerRE.FindStringSubmatch(text)
	if len(match) == 3 {
		return strings.ToUpper(match[1]) + " " + match[2]
	}
	lower := strings.ToLower(text)
	if strings.Contains(lower, "webhook") {
		fields := strings.Fields(text)
		for i, field := range fields {
			if strings.EqualFold(field, "webhook") && i+1 < len(fields) {
				return "webhook " + strings.Trim(fields[i+1], ".,;:")
			}
		}
	}
	if strings.Contains(lower, "stream") {
		fields := strings.Fields(text)
		for i, field := range fields {
			if strings.EqualFold(field, "stream") && i+1 < len(fields) {
				return "stream " + strings.Trim(fields[i+1], ".,;:")
			}
		}
	}
	return ""
}

func inferName(text, trigger string) string {
	lower := strings.ToLower(text)
	fields := strings.Fields(text)
	for i, field := range fields {
		if !strings.EqualFold(strings.Trim(field, ".,;:"), "worker") || i+1 >= len(fields) {
			continue
		}
		candidate := strings.Trim(fields[i+1], ".,;:")
		switch strings.ToLower(candidate) {
		case "that", "which", "who", "qui", "qui,", "receives", "receive", "reçoit", "for":
			continue
		}
		if safeWorkerName(candidate) {
			return candidate
		}
	}
	if strings.Contains(lower, "ticket") && strings.Contains(lower, "triage") {
		return "ticket-triage"
	}
	if trigger != "" {
		parts := strings.Fields(trigger)
		if len(parts) == 2 {
			segment := strings.Trim(parts[1], "/")
			if segment != "" {
				pieces := strings.Split(segment, "/")
				name := strings.ToLower(pieces[len(pieces)-1])
				name = strings.TrimSuffix(name, "s")
				if name != "" {
					return slug(name + "-worker")
				}
			}
		}
	}
	return ""
}

var modelRE = regexp.MustCompile(`(?i)\b(?:model|mod[eè]le|using|avec)\s+([a-z0-9._-]+/[a-z0-9._:+=-]+)`)

func inferModel(text string) string {
	match := modelRE.FindStringSubmatch(text)
	if len(match) == 2 {
		return strings.Trim(match[1], ".,;:")
	}
	return ""
}

var targetRE = regexp.MustCompile(`\b([a-z0-9_]+/[a-z0-9_]+)\b`)

func inferTarget(text string) string {
	match := targetRE.FindStringSubmatch(strings.ToLower(text))
	if len(match) == 2 {
		return match[1]
	}
	return ""
}

func inferDeployEnv(text string) string {
	lower := strings.ToLower(text)
	for _, env := range []string{"staging", "prod", "production", "dev", "preview"} {
		if strings.Contains(lower, env) {
			return env
		}
	}
	return ""
}

func looksLikeCreateWorker(lower string) bool {
	return (strings.Contains(lower, "create") || strings.Contains(lower, "new") || strings.Contains(lower, "build") || strings.Contains(lower, "crée") || strings.Contains(lower, "creer")) && strings.Contains(lower, "worker")
}

func parseCommandOptions(args []string) commandOptions {
	opts := commandOptions{values: map[string]string{}}
	for i := 0; i < len(args); {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			opts.positionals = append(opts.positionals, arg)
			i++
			continue
		}
		key := strings.TrimPrefix(arg, "--")
		if key == "" {
			i++
			continue
		}
		if before, after, ok := strings.Cut(key, "="); ok {
			opts.values[before] = after
			i++
			continue
		}
		if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
			opts.values[key] = "true"
			i++
			continue
		}
		if key == "trigger" && i+2 < len(args) && isHTTPMethod(args[i+1]) && strings.HasPrefix(args[i+2], "/") {
			opts.values[key] = strings.ToUpper(args[i+1]) + " " + args[i+2]
			i += 3
			continue
		}
		opts.values[key] = args[i+1]
		i += 2
	}
	return opts
}

func splitArgs(input string) ([]string, error) {
	var args []string
	var b strings.Builder
	var quote rune
	escaped := false
	flush := func() {
		if b.Len() == 0 {
			return
		}
		args = append(args, b.String())
		b.Reset()
	}
	for _, r := range input {
		switch {
		case escaped:
			b.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				b.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
		case r == ' ' || r == '\t' || r == '\n':
			flush()
		default:
			b.WriteRune(r)
		}
	}
	if escaped {
		b.WriteRune('\\')
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote")
	}
	flush()
	return args, nil
}

func isHTTPMethod(value string) bool {
	switch strings.ToUpper(value) {
	case "GET", "POST":
		return true
	default:
		return false
	}
}

func safeWorkerName(value string) bool {
	if value == "" || value == "." || value == ".." || filepath.Base(value) != value {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

func slug(value string) string {
	value = strings.ToLower(value)
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func operateSlashHelp() string {
	return strings.Join([]string{
		"Operate commands:",
		"/login codex",
		"/new worker <name> --trigger \"POST /tickets\" --model anthropic/claude-sonnet-4-6",
		"/review [subject]",
		"/fix [subject]",
		"/audit",
		"/build --target linux/amd64",
		"/deploy staging",
		"/diff",
		"/read main.go",
		"/docs Tool",
		"/accept-risk rationale",
		"/export",
		"/workers",
		"/tools",
		"/policy",
	}, "\n")
}
