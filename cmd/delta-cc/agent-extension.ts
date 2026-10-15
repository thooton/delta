/**
 * The pi extension behind Delta's backup sessions (see agents.go).
 *
 * pi itself runs on the command center, but everything the model does happens
 * in the session's own container: read, write, edit and bash are replaced by
 * versions that run through `<runtime> exec`. The command center starts pi
 * with these in its environment:
 *
 *   DELTA_AGENT_RUNTIME    the container runtime's CLI (podman or docker)
 *   DELTA_AGENT_CONTAINER  the session's container
 *
 * Nothing from pi's environment is passed into the container: it holds the
 * model's API key.
 */

import { spawn } from "node:child_process";
import { randomUUID } from "node:crypto";
import path from "node:path";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import {
	type BashOperations,
	createBashToolDefinition,
	createEditToolDefinition,
	createReadToolDefinition,
	createWriteToolDefinition,
	type EditOperations,
	type ReadOperations,
	type WriteOperations,
} from "@earendil-works/pi-coding-agent";

const HOME = "/root";
const runtime = process.env.DELTA_AGENT_RUNTIME || "podman";
const container = process.env.DELTA_AGENT_CONTAINER || "";
// A fixed, minimal environment for everything run in the container.
const ENV = ["HOME=" + HOME, "LANG=C.UTF-8", "TERM=dumb"];

type Run = {
	stdin?: string | Buffer;
	onData?: (data: Buffer) => void;
	signal?: AbortSignal;
};

/** Runs argv in the container, resolving with its exit code and stdout (unless onData takes it). */
function run(argv: string[], cwd: string, opts: Run = {}): Promise<{ code: number | null; stdout: Buffer; stderr: string }> {
	return new Promise((resolve, reject) => {
		if (!container) return reject(new Error("no container is set for this session"));
		const args = ["exec", "-i", "-w", cwd];
		for (const e of ENV) args.push("-e", e);
		args.push(container, ...argv);
		const child = spawn(runtime, args, { stdio: ["pipe", "pipe", "pipe"], env: process.env });
		const out: Buffer[] = [];
		let err = "";
		child.stdout.on("data", (d: Buffer) => (opts.onData ? opts.onData(d) : out.push(d)));
		child.stderr.on("data", (d: Buffer) => (opts.onData ? opts.onData(d) : (err += d.toString())));
		child.on("error", reject);
		const onAbort = () => child.kill("SIGKILL");
		opts.signal?.addEventListener("abort", onAbort, { once: true });
		child.on("close", (code) => {
			opts.signal?.removeEventListener("abort", onAbort);
			resolve({ code, stdout: Buffer.concat(out), stderr: err.trim() });
		});
		child.stdin.on("error", () => {});
		child.stdin.end(opts.stdin ?? "");
	});
}

async function must(argv: string[], what: string, stdin?: string | Buffer): Promise<Buffer> {
	const r = await run(argv, HOME, { stdin });
	if (r.code !== 0) throw new Error(r.stderr || `${what} failed`);
	return r.stdout;
}

const readOps: ReadOperations = {
	readFile: (p) => must(["cat", "--", p], `reading ${p}`),
	access: async (p) => {
		await must(["test", "-r", p], `${p} is not readable`);
	},
	detectImageMimeType: async (p) => {
		const ext = path.posix.extname(p).toLowerCase();
		return { ".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp" }[ext] ?? null;
	},
};

const writeOps: WriteOperations = {
	writeFile: async (p, content) => {
		await must(["sh", "-c", 'cat > "$1"', "sh", p], `writing ${p}`, content);
	},
	mkdir: async (dir) => {
		await must(["mkdir", "-p", "--", dir], `creating ${dir}`);
	},
};

const editOps: EditOperations = {
	readFile: readOps.readFile,
	writeFile: writeOps.writeFile,
	access: async (p) => {
		await must(["sh", "-c", 'test -r "$1" && test -w "$1"', "sh", p], `${p} is not readable and writable`);
	},
};

// A command runs as the leader of its own process group, which writes its
// pid where a kill can find it: stopping the `exec` client alone would leave
// the command running in the container.
const bashOps: BashOperations = {
	exec: async (command, cwd, { onData, signal, timeout }) => {
		if (signal?.aborted) throw new Error("aborted");
		const pidFile = `/tmp/.delta-exec-${randomUUID()}`;
		const script = 'echo $$ > "$1"; exec bash -lc "$2"';
		const kill = () =>
			run(["sh", "-c", 'kill -9 -- "-$(cat "$1")" 2>/dev/null; rm -f "$1"', "sh", pidFile], HOME).catch(() => {});
		let timedOut = false;
		let aborted = false;
		const timer =
			timeout && timeout > 0
				? setTimeout(() => {
						timedOut = true;
						kill();
					}, timeout * 1000)
				: undefined;
		const onAbort = () => {
			aborted = true;
			kill();
		};
		signal?.addEventListener("abort", onAbort, { once: true });
		try {
			// setsid notes a killed command on stderr; the timeout or abort says it better.
			const filtered = (d: Buffer) => {
				const text = d.toString();
				if (/^setsid: child \d+ did not exit normally/m.test(text)) {
					const rest = text.replace(/^setsid: child \d+ did not exit normally.*\n?/m, "");
					if (rest) onData(Buffer.from(rest));
				} else onData(d);
			};
			const r = await run(["setsid", "-w", "-f", "bash", "-c", script, "bash", pidFile, command], cwd, { onData: filtered });
			run(["rm", "-f", pidFile], HOME).catch(() => {});
			if (aborted) throw new Error("aborted");
			if (timedOut) throw new Error(`timeout:${timeout}`);
			return { exitCode: r.code };
		} finally {
			if (timer) clearTimeout(timer);
			signal?.removeEventListener("abort", onAbort);
		}
	},
};

export default function (pi: ExtensionAPI) {
	pi.registerTool(createReadToolDefinition(HOME, { operations: readOps }));
	pi.registerTool(createWriteToolDefinition(HOME, { operations: writeOps }));
	pi.registerTool(createEditToolDefinition(HOME, { operations: editOps }));
	pi.registerTool(createBashToolDefinition(HOME, { operations: bashOps }));

	pi.on("user_bash", async () => ({ operations: bashOps }));

	pi.on("before_agent_start", async (event) => {
		event.systemPromptOptions.cwd = `${HOME} (your home directory in this session's Linux container)`;
	});
}
