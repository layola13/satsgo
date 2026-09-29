#!/usr/bin/env node
import readline from "node:readline";

const DEFAULT_PORTS = [43110, 43120, 43121, 43122];
let activeServerUrl = process.env.CODEX_UI_API_URL || null;
const TOKEN = process.env.CODEX_UI_TOKEN || "";

const rl = readline.createInterface({
  input: process.stdin,
  output: process.stdout,
  terminal: false,
});

function send(msg) {
  process.stdout.write(JSON.stringify(msg) + "\n");
}

function logError(msg) {
  process.stderr.write(`[sa-vm-mcp] ${msg}\n`);
}

const SA_VM_ACTIONS = [
  "apply_patch",
  "edit_file",
  "write_file",
  "ensure_dir",
  "mkdir",
  "read_file",
  "read_lines",
  "read_file_tail",
  "count_lines",
  "find_files",
  "list_dir",
  "grep_search",
  "grep_search_async",
  "delete_file",
  "safe_delete",
  "move_file",
  "copy_file",
  "file_info",
  "stat",
  "test_path",
  "file_exists",
  "exists",
  "vm-read",
  "vm_read",
  "git_status",
  "git_diff",
  "clear_tool_history",
  "find_references",
  "find_implementations",
  "find_impl",
  "implementations",
  "file_outline",
  "workspace_outline",
  "repo_map",
  "code_graph",
  "relation_graph",
  "symbol_graph",
  "read_many",
  "verify_loop",
  "verify_changed",
  "rename_symbol",
  "move_file_with_imports",
  "find_definition",
  "extract_to_file",
  "add_import",
  "get_diagnostics",
  "workspace_diagnostics",
  "organize_imports",
  "analyze_scope",
  "run_bun",
  "run_project_check",
  "run_host",
  "exec_host",
  "host",
  "run_js",
  "run_py",
  "http_fetch",
  "jev_choose",
  "jev_checklist",
  "jev_evaluate",
  "jev_thinking",
  "jev_redteam",
  "jev_rank",
  "jev_scope",
  "jev_dispatch",
  "jev_consistency",
  "jev_done",
  "jev_diagnose",
  "jev_edge_cases",
  "jev_blast_radius",
  "save_note",
  "add_note",
  "read_notes",
  "read_note",
  "list_notes",
  "delete_note",
  "remove_note",
  "clear_notes",
];

const SA_VM_TOOL_DEFINITION = {
  name: "sa_vm_run",
  description: `HIGHEST PRIORITY tool for workspace code reading, slicing, line counting, fast grep searching, directory listing, AST refactoring, and code edits: ${SA_VM_ACTIONS.map((a) => `'${a}'`).join(", ")}. Always prefer sa_vm_run over terminal commands (Get-Content, Select-String, Get-ChildItem, Test-Path, cat, grep) because it runs in-process natively in milliseconds, eliminates Windows encoding corruption, and provides automatic token protection.`,
  inputSchema: {
    type: "object",
    required: [],
    properties: {
      action: {
        type: "string",
        enum: SA_VM_ACTIONS,
        description: "Action shortcut: 'read_file', 'read_lines', 'read_many', 'read_file_tail', 'count_lines', 'test_path', 'file_exists', 'exists', 'file_info', 'stat', 'ensure_dir', 'mkdir', 'git_status', 'git_diff', 'edit_file', 'write_file', 'apply_patch', 'find_files', 'list_dir', 'grep_search', 'grep_search_async', 'delete_file', 'safe_delete', 'move_file', 'copy_file', 'find_references', 'find_implementations', 'file_outline', 'workspace_outline', 'repo_map', 'code_graph', 'relation_graph', 'symbol_graph', 'rename_symbol', 'move_file_with_imports', 'find_definition', 'extract_to_file', 'add_import', 'get_diagnostics', 'workspace_diagnostics', 'verify_loop', 'verify_changed', 'save_note', 'read_notes', 'delete_note', 'organize_imports', 'analyze_scope', 'run_host', 'run_bun', 'run_project_check', 'run_js', 'run_py', 'http_fetch', 'jev_choose', 'jev_checklist', 'jev_evaluate', 'jev_thinking', 'jev_redteam', 'jev_rank', 'jev_scope', 'jev_dispatch', 'jev_consistency', 'jev_done', 'jev_diagnose', 'jev_edge_cases', 'jev_blast_radius'."
      },
      path: { type: "string", description: "Target file or directory path relative to workspace (e.g. 'src/main.ts')." },
      file: { type: "string", description: "Target single file path for 'grep_search', 'read_file', 'read_lines', or 'count_lines' (alias of 'path')." },
      dir: { type: "string", description: "Target directory path for 'list_dir', 'grep_search', 'find_files', or 'workspace_diagnostics' (default '.')." },
      type: { type: "string", enum: ["all", "file", "directory"], description: "Filter entries by type for 'list_dir' ('all', 'file', or 'directory')." },
      depth: { type: "integer", description: "Recursion depth for 'list_dir' (default 1; set 2 or 3 to inspect nested subdirectories in a single call)." },
      max_entries: { type: "integer", description: "Maximum entries to return for 'list_dir' or 'find_files' (default 200)." },
      max_files: { type: "integer", description: "Maximum files to scan for 'workspace_diagnostics' (default 100)." },
      max_errors: { type: "integer", description: "Maximum errors to collect for 'workspace_diagnostics' (default 50)." },
      detail: { type: "boolean", description: "Return detailed diagnostics if true, or compact 'path:Lx:Cy:code' if false (default false)." },
      check_references: { type: "boolean", description: "Check symbol references before deleting for 'safe_delete' (default true)." },
      force: { type: "boolean", description: "Force delete for 'safe_delete' even if references exist (default false)." },
      content: { type: "string", description: "File content to write for 'write_file', or script code for 'run_js'/'run_py'." },
      patch: { type: "string", description: "Unified diff patch starting with '*** Begin Patch' and ending with '*** End Patch' for 'apply_patch'." },
      old_text: { type: "string", description: "Exact text to find and replace for 'edit_file'." },
      new_text: { type: "string", description: "Replacement text for 'edit_file'." },
      start_line: { type: "integer", description: "1-based starting line number for 'read_file' / 'read_lines'." },
      end_line: { type: "integer", description: "1-based ending line number for 'read_file' / 'read_lines'." },
      lines: { type: "integer", description: "Trailing lines count for 'read_file_tail' (default 100)." },
      max_tokens: { type: "integer", description: "Max token limit for 'read_file' (default 4000)." },
      numbered: { type: "boolean", description: "Prepend line numbers to read_file output (default true)." },
      pattern: { type: "string", description: "Search glob pattern for 'find_files' (e.g. '*.ts') or search pattern for 'grep_search'." },
      query: { type: "string", description: "Search query text or regex for 'grep_search' or 'grep_search_async'." },
      is_regex: { type: "boolean", description: "Set true if query is a regular expression for 'grep_search' (e.g. '^\\\\s+(load|store)')." },
      queries: { type: "array", items: { type: "string" }, description: "Batch search queries for 'grep_search_async'." },
      max_file_bytes: { type: "integer", description: "Maximum bytes to scan per file for grep actions (default 524288)." },
      max_matches: { type: "integer", description: "Legacy grep result limit; for paged searches, use page_size and count_cap." },
      max_results: { type: "integer", description: "Alias for max_matches on grep actions; page_size controls each paginated response." },
      page: { type: "integer", minimum: 1, description: "1-based result page for grep_search, grep_search_async, find_files, and list_dir." },
      page_size: { type: "integer", minimum: 1, maximum: 500, description: "Number of results in each paginated response." },
      count_cap: { type: "integer", description: "Maximum matches to count for paginated grep before marking the result truncated (default 2000)." },
      exact_total: { type: "boolean", description: "Request exact totals when supported; very large searches may still be bounded by count_cap." },
      symbol: { type: "string", description: "Target symbol name for 'find_definition', 'rename_symbol', or 'file_outline'." },
      old_name: { type: "string", description: "Old symbol name for 'rename_symbol'." },
      new_name: { type: "string", description: "New replacement symbol name for 'rename_symbol'." },
      source: { type: "string", description: "Extraction source file path or SLA bytecode source." },
      target: { type: "string", description: "Target file path for 'extract_to_file' or destination, or symbol/module path for 'code_graph'/'relation_graph'/'symbol_graph' (e.g. class, function, or src/index.ts), or plan/decision under review for 'jev_redteam'." },
      statement: { type: "string", description: "Import statement for 'add_import' (e.g. 'import { t } from \"./i18n\";')." },
      component_name: { type: "string", description: "Component name for 'analyze_scope' or 'extract_to_file'." },
      profile: { type: "string", description: "Predefined project check profile (e.g. 'web-typecheck', 'server-typecheck')." },
      subcommand: { type: "string", description: "Subcommand for 'run_bun' (e.g. 'test', 'build')." },
      exe: { type: "string", description: "Target executable binary for 'run_host'. ALLOWLIST ONLY: bun, node, python, zig, cargo, go, rustc, git (plus workspace-internal paths). FORBIDDEN: bash, pwd, sh, powershell, pwsh, cmd, curl. Use native list_dir/find_files/git_status instead of shell." },
      args: { type: "array", items: { type: "string" }, description: "Command-line arguments for 'run_host'." },
      head_lines: { type: "integer", description: "First N lines of output to retain for 'run_host'." },
      tail_lines: { type: "integer", description: "Last N lines of output to retain for 'run_host'." },
      env: { type: "object", description: "Allowlisted environment variables for 'run_host' (e.g. SA_PLUGIN_DEV, ZIG_EXE, NO_COLOR)." },
      key: { type: "string", description: "Identifier key for 'save_note', 'read_notes', or 'delete_note' (e.g. 'test_rule')." },
      fact: { type: "string", description: "Fact or note content for 'save_note' (alias of 'content')." },
      level: { type: "string", enum: ["low", "normal", "important", "critical"], description: "Priority level for 'save_note' ('low', 'normal', 'important', 'critical'; default 'normal'). Higher priority notes are sorted first and protected from eviction." },
      all: { type: "boolean", description: "Clear all notes for 'delete_note' (default false)." },
      from: { type: "string", description: "Source path for 'move_file' or 'move_file_with_imports'." },
      to: { type: "string", description: "Target path for 'move_file' or 'move_file_with_imports'." },
      cwd: { type: "string", description: "Workspace directory root. Defaults to current working directory." },
      threadId: { type: "string", description: "Optional conversation thread ID if known." },
      question: { type: "string", description: "Decision question for 'jev_choose' (2-8 options), goal fallback for 'jev_thinking'." },
      options: { type: "object", description: "Choice options map for 'jev_choose' (e.g. { a: 'desc A', b: 'desc B' }).", additionalProperties: true },
      checks: { type: "object", description: "Checklist items map for 'jev_checklist' (e.g. { no_placeholders: '...' }).", additionalProperties: true },
      questions: { type: "object", description: "Custom questions map for 'jev_evaluate' passthrough.", additionalProperties: true },
      goal: { type: "string", description: "Thinking goal for 'jev_thinking'." },
      thoughts: { type: "object", description: "Candidate lines of thought for 'jev_thinking' (1-4 entries; '__act_now' brake is built in).", additionalProperties: true },
      context: { type: "string", description: "Supporting context for jev_* advisory actions (falls back to 'content')." },
      items: { type: "object", description: "Items map to rank for 'jev_rank' (2-6 entries).", additionalProperties: true },
      criterion: { type: "string", description: "Ranking criterion for 'jev_rank' (e.g. 'ROI')." },
      task: { type: "string", description: "Task under scope triage for 'jev_scope'." },
      phases: { type: "object", description: "Candidate split phases for 'jev_scope' (up to 6 entries).", additionalProperties: true },
      plan: { type: "string", description: "Plan under consistency audit for 'jev_consistency'." },
      constraints: { type: "object", description: "Hard constraints map for 'jev_consistency' (1-8 entries).", additionalProperties: true },
      user_request: { type: "string", description: "Original user request for 'jev_done'." },
      actual_work: { type: "string", description: "Actual work performed for 'jev_done'." },
      evidence: { type: "string", description: "Verification evidence for 'jev_done'." },
      error_summary: { type: "string", description: "Error or failure summary for 'jev_diagnose'." },
      hypotheses: { type: "object", description: "Root-cause hypotheses map for 'jev_diagnose' (2-4 entries).", additionalProperties: true },
      proposed_fix: { type: "string", description: "Proposed fix under judgment for 'jev_diagnose'." },
      code: { type: "string", description: "Code under boundary review for 'jev_edge_cases'." },
      diff: { type: "string", description: "Unified diff under blast-radius review for 'jev_blast_radius'." },
      target_component: { type: "string", description: "Target component of the diff for 'jev_blast_radius'." },
      mode: { type: "string", description: "Code graph mode for 'code_graph'/'relation_graph'/'symbol_graph' ('symbol_hierarchy', 'module_dependency'/'module_deps', 'call_flow')." },
      direction: { type: "string", description: "Dependency direction for 'code_graph' module_dependency ('upstream', 'downstream', 'both')." },
      format: { type: "string", description: "Output format for 'code_graph' ('dot', 'py_stub', 'networkx')." },
      output_file: { type: "string", description: "Optional output file for 'code_graph' (default pure memory)." },
      candidate_files: { type: "array", items: { type: "string" }, description: "Optional candidate files to restrict 'code_graph' scope." },
      files: { type: "array", items: { type: "string" }, description: "File list for 'read_many' (max 10 files in one call)." },
      staged: { type: "boolean", description: "Show staged changes for 'git_diff' (default unstaged+untracked summary)." },
      stat_only: { type: "boolean", description: "Only file stats for 'git_diff'." },
      pathspec: { type: "string", description: "Path filter for 'git_diff' (single file/dir)." },
      context: { type: "integer", description: "Diff context lines for 'git_diff' (0-10)." },
      parallel: { type: "array", description: "Batch tasks for parallel execution (max 32). CORRECT shape: { parallel: [{ action: 'list_dir', dir: '.' }], concurrency: 4 } with NO action:'parallel'. NEVER send action:'parallel'; omit top-level action when parallel is present." },
      concurrency: { type: "integer", description: "Concurrency for parallel batch (1-8, default = task count)." }
    }
  }
};

async function fetchSaVmEndpoint(effectiveArgs) {
  const headers = { "Content-Type": "application/json" };
  if (TOKEN) {
    headers["Authorization"] = `Bearer ${TOKEN}`;
  }

  const urlsToTry = [];
  if (activeServerUrl) {
    urlsToTry.push(activeServerUrl);
  }
  for (const port of DEFAULT_PORTS) {
    const candidate = `http://127.0.0.1:${port}`;
    if (!urlsToTry.includes(candidate)) {
      urlsToTry.push(candidate);
    }
  }

  let lastError = null;
  let authBlockedUrl = null;
  let authBlockedCode = "";
  for (const baseUrl of urlsToTry) {
    try {
      const controller = new AbortController();
      const timer = setTimeout(() => controller.abort(), 2000);
      const res = await fetch(`${baseUrl}/api/tools/sa_vm_run`, {
        method: "POST",
        headers,
        body: JSON.stringify(effectiveArgs),
        signal: controller.signal
      });
      clearTimeout(timer);
      if (res.status === 401) {
        // A 401 is credential-scoped, not server-scoped: never latch onto it.
        // Keep probing the remaining candidates for a server that accepts our
        // credential (e.g. a trusted-local or token-less instance) instead of
        // sticking to the first responder forever (CUX-AUTH-001 lock-in).
        if (!authBlockedUrl) {
          authBlockedUrl = baseUrl;
          try {
            const match = /"code"\s*:\s*"([^"]+)"/.exec(await res.text());
            if (match) authBlockedCode = match[1];
          } catch {}
        }
        continue;
      }
      if (res.status !== 404 && res.status !== 502 && res.status !== 503) {
        activeServerUrl = baseUrl;
        return { response: res, url: baseUrl };
      }
    } catch (err) {
      lastError = err;
    }
  }

  if (authBlockedUrl) {
    throw new Error(
      `Codex Ultra server at ${authBlockedUrl} rejected the sa-vm credential (401 ${authBlockedCode || "CUX-AUTH-001"}). ` +
      `Set CODEX_UI_TOKEN on the server and in this MCP client env, or run the server with trusted local desktop auth (CODEX_UI_DESKTOP + CODEX_UI_TRUSTED_LOCAL_AUTH) / CODEX_UI_AUTH=off for local use.`
    );
  }
  throw lastError || new Error(`Unable to connect to Codex Ultra server on any candidate port (${DEFAULT_PORTS.join(", ")})`);
}

rl.on("line", async (line) => {
  const trimmed = line.trim();
  if (!trimmed) return;

  let req;
  try {
    req = JSON.parse(trimmed);
  } catch (err) {
    logError(`Invalid JSON: ${err.message}`);
    return;
  }

  const { id, method, params } = req;
  // If no id, it's a notification, ignore per JSON-RPC 2.0
  if (id === undefined || id === null) {
    return;
  }

  if (method === "initialize") {
    send({
      jsonrpc: "2.0",
      id,
      result: {
        protocolVersion: "2024-11-05",
        capabilities: {
          tools: { listChanged: false }
        },
        serverInfo: {
          name: "sa-vm",
          version: "1.0.0"
        }
      }
    });
    return;
  }

  if (method === "ping") {
    send({ jsonrpc: "2.0", id, result: {} });
    return;
  }

  if (method === "tools/list") {
    send({
      jsonrpc: "2.0",
      id,
      result: {
        tools: [SA_VM_TOOL_DEFINITION]
      }
    });
    return;
  }

  if (method === "tools/call") {
    if (params?.name !== "sa_vm_run") {
      send({
        jsonrpc: "2.0",
        id,
        error: { code: -32601, message: `Tool '${params?.name}' not found` }
      });
      return;
    }

    const rawArgs = params.arguments || {};
    const effectiveArgs = {
      ...rawArgs,
      cwd: rawArgs.cwd || process.cwd()
    };

    try {
      const { response, url: connectedUrl } = await fetchSaVmEndpoint(effectiveArgs);

      const resJson = await response.json().catch(() => ({}));
      const data = resJson?.data;

      let outputText = "";
      if (typeof data?.stdout === "string" && data.stdout) {
        outputText = data.stdout;
        if (data.stderr) {
          outputText += `\n[stderr]\n${data.stderr}`;
        }
      } else if (typeof data?.output === "string") {
        outputText = data.output;
      } else if (resJson?.error) {
        outputText = `Error: ${resJson.error} (code: ${resJson.code || "UNKNOWN"})`;
      } else {
        outputText = JSON.stringify(data || resJson, null, 2);
      }

      const isError = !response.ok || data?.ok === false || data?.kind === "sa_error" || data?.kind === "capability_denied";

      send({
        jsonrpc: "2.0",
        id,
        result: {
          content: [
            {
              type: "text",
              text: outputText
            }
          ],
          isError
        }
      });
    } catch (err) {
      logError(`Fetch failed: ${err.message}`);
      send({
        jsonrpc: "2.0",
        id,
        result: {
          content: [
            {
              type: "text",
              text: `[SA-VM Connection Failure]: Unable to reach Codex Ultra server at ${activeServerUrl || "http://127.0.0.1:43110"} (${err.message}).\nPlease ensure the Codex Ultra service is running.`
            }
          ],
          isError: true
        }
      });
    }
    return;
  }

  send({
    jsonrpc: "2.0",
    id,
    error: {
      code: -32601,
      message: `Method '${method}' not supported`
    }
  });
});
