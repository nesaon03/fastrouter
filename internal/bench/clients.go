package bench

import (
	"fmt"
	"strings"
)

// Profile is one IDE or CLI the Speed Test can imitate: the endpoint it calls,
// its headers, the size and style of its system prompt and the tools it
// declares. Prompts are condensed but keep each client's structure, so
// FastRouter (and the upstream) do the same work they do for the real client.
type Profile struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Logo      string `json:"logo,omitempty"` // file under /logos/
	Kind      string `json:"kind"`           // ide | cli | api
	Endpoint  string `json:"endpoint"`       // chat | messages | responses | gemini
	UserAgent string `json:"userAgent"`
	Tools     int    `json:"tools"`
	Note      string `json:"note,omitempty"`

	headers   map[string]string
	system    string
	tools     []string
	xmlTools  bool   // tools described inside the system prompt (Cline family)
	maxTokens int    // what the client sends; 0 = omits it
	effort    string // reasoning effort the client asks for by default
	preamble  string // context block the client sends before the user's text
	extra     map[string]any
}

const benchUA = " fastrouter-bench/1"

// Profiles lists the clients in display order. Raw API profiles come last.
var Profiles = []*Profile{
	{ID: "cursor", Name: "Cursor", Logo: "cursor.png", Kind: "ide", Endpoint: Chat,
		UserAgent: "Cursor/1.7.46 (Windows_NT 10.0.19045; x64)" + benchUA,
		system:    agentPrompt("You are an AI coding assistant, powered by the selected model. You operate in Cursor.", "tool_calling", "making_code_changes", "communication"),
		tools:     []string{"codebase_search", "read_file", "run_terminal_cmd", "list_dir", "grep_search", "edit_file", "file_search", "delete_file", "web_search", "todo_write"},
		preamble:  "<additional_data>\nBelow are some potentially helpful pieces of information for responding to the user's request.\n<current_file>\nPath: internal/config/config.go\nLine: 18\n</current_file>\n</additional_data>",
		extra:     map[string]any{"tool_choice": "auto", "parallel_tool_calls": true}},
	{ID: "claude", Name: "Claude Code", Logo: "claude.png", Kind: "cli", Endpoint: Messages,
		UserAgent: "claude-code/2.1.31 (external, cli)" + benchUA, maxTokens: 32000,
		headers: map[string]string{"anthropic-version": "2023-06-01", "x-app": "cli",
			"anthropic-beta": "interleaved-thinking-2025-05-14,fine-grained-tool-streaming-2025-05-14"},
		system:   agentPrompt("You are Claude Code, Anthropic's official CLI for Claude. You are an interactive CLI tool that helps users with software engineering tasks.", "tone_and_style", "task_management", "tool_usage_policy"),
		tools:    []string{"Task", "Bash", "Glob", "Grep", "Read", "Edit", "Write", "TodoWrite", "WebFetch", "WebSearch"},
		preamble: "<system-reminder>\nAs you answer the user's questions, you can use the following context:\n# claudeMd\nCodebase and user instructions are shown below.\n- Go project, run go test ./... before committing.\n</system-reminder>"},
	{ID: "codex", Name: "Codex CLI", Logo: "codex.png", Kind: "cli", Endpoint: Responses,
		UserAgent: "codex_cli_rs/0.58.0 (Windows 10.0.19045; x86_64) WindowsTerminal" + benchUA, effort: "medium",
		headers:  map[string]string{"originator": "codex_cli_rs", "version": "0.58.0", "session_id": "fastrouter-bench-session"},
		system:   agentPrompt("You are Codex, based on GPT-5. You are running as a coding agent in the Codex CLI on a user's computer.", "general", "editing_constraints", "presenting_your_work"),
		tools:    []string{"shell", "apply_patch", "update_plan", "view_image"},
		preamble: "<environment_context>\n  <cwd>C:\\Users\\dev\\project</cwd>\n  <approval_policy>on-request</approval_policy>\n  <sandbox_mode>workspace-write</sandbox_mode>\n  <shell>powershell</shell>\n</environment_context>",
		extra:    map[string]any{"tool_choice": "auto", "parallel_tool_calls": false, "store": false, "include": []string{"reasoning.encrypted_content"}, "prompt_cache_key": "fastrouter-bench-session"}},
	{ID: "cline", Name: "Cline", Logo: "cline.png", Kind: "ide", Endpoint: Chat, xmlTools: true,
		UserAgent: "OpenAI/JS 4.104.0" + benchUA,
		headers:   map[string]string{"HTTP-Referer": "https://cline.bot", "X-Title": "Cline"},
		system:    agentPrompt("You are Cline, a highly skilled software engineer with extensive knowledge in many programming languages, frameworks, design patterns, and best practices.", "rules", "objective"),
		tools:     []string{"execute_command", "read_file", "write_to_file", "replace_in_file", "search_files", "list_files", "list_code_definition_names", "ask_followup_question", "attempt_completion"},
		preamble:  clineEnv, extra: map[string]any{"temperature": 0}},
	{ID: "roo", Name: "Roo Code", Logo: "roo.png", Kind: "ide", Endpoint: Chat, xmlTools: true,
		UserAgent: "OpenAI/JS 5.12.2" + benchUA,
		headers:   map[string]string{"HTTP-Referer": "https://github.com/RooVetGit/Roo-Cline", "X-Title": "Roo Code"},
		system:    agentPrompt("You are Roo, a highly skilled software engineer with extensive knowledge in many programming languages, frameworks, design patterns, and best practices.", "rules", "modes", "objective"),
		tools:     []string{"read_file", "search_files", "list_files", "list_code_definition_names", "apply_diff", "write_to_file", "execute_command", "ask_followup_question", "attempt_completion", "switch_mode", "new_task"},
		preamble:  clineEnv, extra: map[string]any{"temperature": 0}},
	{ID: "kilo", Name: "Kilo Code", Logo: "kilocode.png", Kind: "ide", Endpoint: Chat, xmlTools: true,
		UserAgent: "Kilo-Code/4.96.0" + benchUA,
		headers:   map[string]string{"HTTP-Referer": "https://kilocode.ai", "X-Title": "Kilo Code"},
		system:    agentPrompt("You are Kilo Code, a highly skilled software engineer with extensive knowledge in many programming languages, frameworks, design patterns, and best practices.", "rules", "modes", "objective"),
		tools:     []string{"read_file", "search_files", "list_files", "list_code_definition_names", "apply_diff", "write_to_file", "execute_command", "ask_followup_question", "attempt_completion", "switch_mode", "new_task"},
		preamble:  clineEnv, extra: map[string]any{"temperature": 0}},
	{ID: "continue", Name: "Continue", Logo: "continue.png", Kind: "ide", Endpoint: Chat,
		UserAgent: "Continue/1.2.10 (vscode)" + benchUA,
		system:    agentPrompt("You are in agent mode. Use the available tools when they help; otherwise answer directly. Always use markdown code blocks with the file path.", "communication"),
		tools:     []string{"builtin_read_file", "builtin_edit_existing_file", "builtin_create_new_file", "builtin_run_terminal_command", "builtin_grep_search", "builtin_file_glob_search", "builtin_ls"},
		extra:     map[string]any{"tool_choice": "auto"}},
	{ID: "opencode", Name: "OpenCode", Logo: "opencode.png", Kind: "cli", Endpoint: Chat, maxTokens: 32000,
		UserAgent: "opencode/0.15.8 ai-sdk/openai-compatible/1.0.19" + benchUA,
		system:    agentPrompt("You are opencode, an interactive CLI tool that helps users with software engineering tasks.", "tone_and_style", "task_management", "tool_usage_policy"),
		tools:     []string{"bash", "edit", "glob", "grep", "list", "read", "write", "todowrite", "task", "webfetch"}},
	{ID: "gemini-cli", Name: "Gemini CLI", Logo: "gemini-cli.png", Kind: "cli", Endpoint: Gemini,
		UserAgent: "GeminiCLI/0.12.0 (win32; x64)" + benchUA,
		system:    agentPrompt("You are an interactive CLI agent specializing in software engineering tasks. Your primary goal is to help users safely and efficiently.", "core_mandates", "primary_workflows"),
		tools:     []string{"list_directory", "read_file", "search_file_content", "glob", "replace", "write_file", "web_fetch", "run_shell_command", "save_memory", "google_web_search"},
		extra:     map[string]any{"generationConfig": map[string]any{"temperature": 0, "topP": 1, "thinkingConfig": map[string]any{"includeThoughts": true}}}},
	{ID: "hermes", Name: "Hermes Agent", Logo: "hermes.png", Kind: "cli", Endpoint: Chat,
		UserAgent: "OpenAI/Python 1.109.1" + benchUA,
		system:    agentPrompt("You are Hermes Agent, an intelligent AI assistant created by Nous Research. You are helpful, knowledgeable, and direct.", "tool_usage_policy", "communication"),
		tools:     []string{"terminal", "read_file", "write_file", "patch", "search_files", "web_search", "web_extract", "todo", "memory", "skills_list"}},
	{ID: "copilot", Name: "GitHub Copilot", Logo: "copilot.png", Kind: "ide", Endpoint: Chat,
		UserAgent: "GitHubCopilotChat/0.32.4" + benchUA,
		headers:   map[string]string{"Editor-Version": "vscode/1.105.1", "Editor-Plugin-Version": "copilot-chat/0.32.4", "Copilot-Integration-Id": "vscode-chat", "Openai-Intent": "conversation-agent"},
		system:    agentPrompt("You are an expert AI programming assistant, working with a user in the VS Code editor. When asked for your name, you must respond with \"GitHub Copilot\".", "tool_calling", "making_code_changes"),
		tools:     []string{"semantic_search", "file_search", "grep_search", "read_file", "list_dir", "create_file", "replace_string_in_file", "run_in_terminal", "get_errors"},
		extra:     map[string]any{"tool_choice": "auto"}},
	{ID: "droid", Name: "Factory Droid", Logo: "droid.png", Kind: "cli", Endpoint: Chat, maxTokens: 32000,
		UserAgent: "factory-cli/0.22.3" + benchUA,
		system:    agentPrompt("You are Droid, an AI software engineering agent built by Factory.", "general", "tool_usage_policy"),
		tools:     []string{"Read", "LS", "Execute", "Edit", "MultiEdit", "Create", "Grep", "Glob", "TodoWrite", "WebSearch"}},
	{ID: "crush", Name: "Crush", Logo: "crush.png", Kind: "cli", Endpoint: Chat,
		UserAgent: "OpenAI/Go 2.7.1" + benchUA,
		system:    agentPrompt("You are Crush, a powerful AI Assistant that runs in the CLI.", "tone_and_style", "tool_usage_policy"),
		tools:     []string{"bash", "edit", "multiedit", "fetch", "glob", "grep", "ls", "view", "write"}},
	{ID: "qwen", Name: "Qwen Code", Logo: "qwen.png", Kind: "cli", Endpoint: Chat,
		UserAgent: "QwenCode/0.2.1 (win32; x64)" + benchUA,
		system:    agentPrompt("You are Qwen Code, an interactive CLI agent developed by Alibaba Group, specializing in software engineering tasks.", "core_mandates", "primary_workflows"),
		tools:     []string{"list_directory", "read_file", "search_file_content", "glob", "replace", "write_file", "run_shell_command", "todo_write", "save_memory"}},

	{ID: "api-chat", Name: "Chat API", Kind: "api", Endpoint: Chat, UserAgent: "fastrouter-bench/1", Note: "Plain request, no system prompt or tools"},
	{ID: "api-messages", Name: "Messages API", Kind: "api", Endpoint: Messages, UserAgent: "fastrouter-bench/1", Note: "Plain request, no system prompt or tools",
		headers: map[string]string{"anthropic-version": "2023-06-01"}},
	{ID: "api-responses", Name: "Responses API", Kind: "api", Endpoint: Responses, UserAgent: "fastrouter-bench/1", Note: "Plain request, no system prompt or tools"},
	{ID: "api-gemini", Name: "Gemini API", Kind: "api", Endpoint: Gemini, UserAgent: "fastrouter-bench/1", Note: "Plain request, no system prompt or tools"},
}

func init() {
	for _, p := range Profiles {
		p.Tools = len(p.tools)
	}
}

// ProfileByID finds a client profile ("" = nil).
func ProfileByID(id string) *Profile {
	for _, p := range Profiles {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// apiProfile is the raw-API profile for an endpoint.
func apiProfile(ep string) *Profile { return ProfileByID("api-" + ep) }

const clineEnv = "<environment_details>\n# VSCode Visible Files\ninternal/config/config.go\n\n# Current Time\n17/11/2026, 10:12:04 am (Asia/Singapore, UTC+8:00)\n\n# Current Mode\nACT MODE\n</environment_details>"

// Prompt sections shared by the agent prompts. Each client's prompt is its
// own intro plus the sections it uses, then its environment block.
var sections = map[string]string{
	"tool_calling": `<tool_calling>
You have tools at your disposal to solve the coding task. Follow these rules regarding tool calls:
1. ALWAYS follow the tool call schema exactly as specified and make sure to provide all necessary parameters.
2. NEVER refer to tool names when speaking to the USER. Say what the tool is doing in natural language.
3. Only call tools when they are necessary. If the USER's task is general or you already know the answer, respond without calling tools.
4. Before calling each tool, first explain to the USER why you are calling it.
</tool_calling>`,
	"making_code_changes": `<making_code_changes>
When making code changes, NEVER output code to the USER unless requested. Use one of the code edit tools instead.
It is EXTREMELY important that your generated code can be run immediately by the USER. Add all necessary import statements, dependencies and endpoints. Never generate an extremely long hash or any non-textual code. Unless you are appending a small easy edit, read the contents or section of what you're editing before editing it. Fix linter errors if clear how to; do not loop more than 3 times on the same file.
</making_code_changes>`,
	"communication": `<communication>
Be conversational but professional. Format your responses in markdown. Use backticks to format file, directory, function and class names. NEVER lie or make things up. Refrain from apologizing all the time when results are unexpected.
</communication>`,
	"tone_and_style": `# Tone and style
You should be concise, direct, and to the point. Your output will be displayed on a command line interface. Your responses can use GitHub-flavored markdown, rendered in a monospace font. Output text to communicate with the user; all text you output outside of tool use is displayed to the user. Only use tools to complete tasks. Minimize output tokens as much as possible while maintaining helpfulness, quality, and accuracy. Do not add unnecessary preamble or postamble unless the user asks you to.`,
	"task_management": `# Task Management
You have access to the TodoWrite tool to help you manage and plan tasks. Use it VERY frequently to make sure that you are tracking your tasks and giving the user visibility into your progress. Mark todos as completed as soon as you are done with a task. Do not batch up multiple tasks before marking them as completed.`,
	"tool_usage_policy": `# Tool usage policy
- When doing file search, prefer the Task tool to reduce context usage.
- You can call multiple tools in a single response. When multiple independent pieces of information are requested, batch your tool calls together for optimal performance.
- When making multiple bash tool calls, send a single message with multiple tool calls to run them in parallel.
- Answer directly, without tools, when the request is a question you can answer from the given context.`,
	"general": `## General
- The arguments to shell are passed to execvp(). Most terminal commands should be prefixed with ["powershell", "-Command"].
- Always set the workdir param when using the shell function. Do not use cd unless absolutely necessary.
- When searching for text or files, prefer rg or rg --files because rg is much faster than alternatives like grep.`,
	"editing_constraints": `## Editing constraints
- Default to ASCII when editing or creating files. Only introduce non-ASCII characters when there is a clear justification.
- Add succinct code comments that explain what is going on if code is not self-explanatory.
- You may be in a dirty git worktree. NEVER revert existing changes you did not make unless explicitly requested.
- Use apply_patch for single file edits.`,
	"presenting_your_work": `## Presenting your work and final message
You are producing plain text that will later be styled by the CLI. Be very concise; friendly coding teammate tone. Ask only when needed; suggest ideas; mirror the user's style. If there's something that you think you could help with as a logical next step, concisely ask the user if they want you to do so.`,
	"rules": `====

RULES

- The current working directory is: C:/Users/dev/project
- You cannot cd into a different directory to complete a task. You are stuck operating from the current working directory.
- Do not use the ~ character or $HOME to refer to the home directory.
- Before using the execute_command tool, you must first think about the SYSTEM INFORMATION context provided to understand the user's environment.
- When using the search_files tool, craft your regex patterns carefully to balance specificity and flexibility.
- Use one tool per message, and wait for the result of each tool use before continuing.
- You are STRICTLY FORBIDDEN from starting your messages with "Great", "Certainly", "Okay", "Sure".

====

SYSTEM INFORMATION

Operating System: Windows 10
Default Shell: C:\WINDOWS\System32\WindowsPowerShell\v1.0\powershell.exe
Home Directory: C:/Users/dev
Current Working Directory: C:/Users/dev/project`,
	"modes": `====

MODES

- These are the currently available modes:
  * "Code" mode (code) - Use this mode when you need to write, modify, or refactor code.
  * "Ask" mode (ask) - Use this mode when you need explanations, documentation, or answers to technical questions.
  * "Architect" mode (architect) - Use this mode when you need to plan, design, or strategize before implementation.
  * "Debug" mode (debug) - Use this mode when you're troubleshooting issues or investigating errors.`,
	"objective": `====

OBJECTIVE

You accomplish a given task iteratively, breaking it down into clear steps and working through them methodically.
1. Analyze the user's task and set clear, achievable goals to accomplish it.
2. Work through these goals sequentially, utilizing available tools one at a time as necessary.
3. Once you've completed the user's task, you must use the attempt_completion tool to present the result of the task to the user.`,
	"core_mandates": `# Core Mandates

- **Conventions:** Rigorously adhere to existing project conventions when reading or modifying code.
- **Libraries/Frameworks:** NEVER assume a library/framework is available or appropriate. Verify its established usage within the project.
- **Style & Structure:** Mimic the style, structure, framework choices, typing, and architectural patterns of existing code.
- **Comments:** Add code comments sparingly. Focus on *why* something is done.
- **Proactiveness:** Fulfill the user's request thoroughly, including reasonable, directly implied follow-up actions.`,
	"primary_workflows": `# Primary Workflows

## Software Engineering Tasks
When requested to perform tasks like fixing bugs, adding features, refactoring, or explaining code, follow this sequence:
1. **Understand:** Think about the user's request and the relevant codebase context.
2. **Plan:** Build a coherent and grounded plan for how you intend to resolve the user's task.
3. **Implement:** Use the available tools to act on the plan.
4. **Verify (Tests):** If applicable and feasible, verify the changes using the project's testing procedures.`,
}

const envBlock = `<env>
Working directory: C:\Users\dev\project
Is directory a git repo: Yes
Platform: win32
OS Version: Windows 10.0.19045
Today's date: 2026-11-17
</env>`

func agentPrompt(intro string, names ...string) string {
	parts := []string{intro}
	for _, n := range names {
		parts = append(parts, sections[n])
	}
	return strings.Join(append(parts, envBlock), "\n\n")
}

// toolDefs: name -> description | params. Params are "name" (string),
// "name#" (integer), "name?" (boolean), "name[]" (string array); a leading
// "*" marks a required parameter.
var toolDefs = map[string][2]string{}

func def(desc, params string, names ...string) {
	for _, n := range names {
		toolDefs[n] = [2]string{desc, params}
	}
}

func init() {
	def("Read a file from the workspace. Returns its contents with line numbers.", "*path offset# limit#",
		"read_file", "Read", "read", "view", "builtin_read_file")
	def("Write a file, replacing it if it exists.", "*path *content", "write_to_file", "Write", "write", "write_file", "Create", "create_file", "builtin_create_new_file")
	def("Replace an exact string in a file with new text.", "*path *old_string *new_string replace_all?",
		"edit_file", "Edit", "edit", "replace", "replace_in_file", "replace_string_in_file", "patch", "builtin_edit_existing_file")
	def("Apply several exact-string edits to one file.", "*path *edits[]", "MultiEdit", "multiedit")
	def("Apply a unified diff to a file.", "*path *diff", "apply_diff")
	def("Apply a patch in the apply_patch format to files in the workspace.", "*input", "apply_patch")
	def("Run a shell command and return its output.", "*command workdir timeout_ms# description",
		"run_terminal_cmd", "Bash", "bash", "shell", "execute_command", "run_shell_command", "terminal", "Execute", "run_in_terminal", "builtin_run_terminal_command")
	def("Search file contents with a regular expression.", "*pattern path glob output_mode",
		"grep_search", "Grep", "grep", "search_files", "search_file_content", "builtin_grep_search")
	def("Find files by glob pattern.", "*pattern path", "Glob", "glob", "file_search", "builtin_file_glob_search")
	def("List the files in a directory.", "*path recursive?", "list_dir", "list_files", "LS", "ls", "list", "list_directory", "builtin_ls")
	def("Semantic search over the codebase for code relevant to a question.", "*query directories[]", "codebase_search", "semantic_search")
	def("List top-level definitions (classes, functions) in a directory.", "*path", "list_code_definition_names")
	def("Delete a file.", "*path", "delete_file")
	def("Search the web and return result snippets.", "*query", "web_search", "WebSearch", "google_web_search")
	def("Fetch a URL and return its content as markdown.", "*url prompt", "WebFetch", "webfetch", "web_fetch", "fetch", "web_extract")
	def("Create and manage a structured task list for the session.", "*todos[]", "todo_write", "TodoWrite", "todowrite", "todo", "update_plan")
	def("Launch a sub-agent to handle a complex multi-step task.", "*description *prompt subagent_type", "Task", "task", "new_task")
	def("Show a local image to the model.", "*path", "view_image")
	def("Ask the user a question to gather more information.", "*question options[]", "ask_followup_question")
	def("Present the result of the task to the user.", "*result command", "attempt_completion")
	def("Switch to a different mode.", "*mode_slug reason", "switch_mode")
	def("Get compile or lint errors for files.", "filePaths[]", "get_errors")
	def("Save a fact to long-term memory.", "*fact", "save_memory", "memory")
	def("List the skills available to the agent.", "", "skills_list")
}

func toolSchema(name string) (string, map[string]any) {
	d, ok := toolDefs[name]
	if !ok {
		panic("bench: no tool " + name)
	}
	props, req := map[string]any{}, []string{}
	for _, p := range strings.Fields(d[1]) {
		if strings.HasPrefix(p, "*") {
			p = p[1:]
			req = append(req, strings.TrimRight(p, "#?[]"))
		}
		switch {
		case strings.HasSuffix(p, "#"):
			props[p[:len(p)-1]] = num(strings.ReplaceAll(p[:len(p)-1], "_", " "))
		case strings.HasSuffix(p, "?"):
			props[p[:len(p)-1]] = boolean(strings.ReplaceAll(p[:len(p)-1], "_", " "))
		case strings.HasSuffix(p, "[]"):
			props[p[:len(p)-2]] = strs(strings.ReplaceAll(p[:len(p)-2], "_", " "))
		default:
			props[p] = str(strings.ReplaceAll(p, "_", " "))
		}
	}
	return d[0], map[string]any{"type": "object", "properties": props, "required": req}
}

// xmlToolDocs renders tools the Cline way: described in the system prompt,
// called with XML tags instead of native tool calls.
func xmlToolDocs(names []string) string {
	var b strings.Builder
	b.WriteString("====\n\nTOOL USE\n\nYou have access to a set of tools that are executed upon the user's approval. You can use one tool per message. Tool use is formatted using XML-style tags:\n\n<tool_name>\n<parameter1_name>value1</parameter1_name>\n</tool_name>\n\n# Tools\n")
	for _, n := range names {
		desc, s := toolSchema(n)
		fmt.Fprintf(&b, "\n## %s\nDescription: %s\nParameters:\n", n, desc)
		props := s["properties"].(map[string]any)
		usage := ""
		for _, f := range strings.Fields(toolDefs[n][1]) {
			req := strings.HasPrefix(f, "*")
			f = strings.TrimRight(strings.TrimPrefix(f, "*"), "#?[]")
			tag := "optional"
			if req {
				tag = "required"
			}
			fmt.Fprintf(&b, "- %s: (%s) %s\n", f, tag, props[f].(map[string]any)["description"])
			usage += fmt.Sprintf("<%s>%s here</%s>\n", f, f, f)
		}
		fmt.Fprintf(&b, "Usage:\n<%s>\n%s</%s>\n", n, usage, n)
	}
	return b.String()
}

// ClientIDs lists the profile ids, comma-separated.
func ClientIDs() string {
	ids := make([]string, len(Profiles))
	for i, p := range Profiles {
		ids[i] = p.ID
	}
	return strings.Join(ids, ", ")
}
