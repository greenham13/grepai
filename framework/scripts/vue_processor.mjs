import path from "node:path";
import { createRequire } from "node:module";
import { pathToFileURL } from "node:url";
import { readFileSync } from "node:fs";

const chunks = [];
for await (const chunk of process.stdin) {
  chunks.push(chunk);
}

const input = JSON.parse(Buffer.concat(chunks).toString("utf8"));
const source = input.source ?? "";
const filePath = input.filePath ?? "Component.vue";
const absoluteFilePath = path.isAbsolute(filePath) ? filePath : path.resolve(process.cwd(), filePath);

function candidateResolveDirs(startFilePath) {
  const dirs = [];
  let dir = path.dirname(startFilePath);
  while (true) {
    dirs.push(dir);
    const parent = path.dirname(dir);
    if (parent === dir) break;
    dir = parent;
  }
  if (!dirs.includes(process.cwd())) {
    dirs.push(process.cwd());
  }
  return dirs;
}

// The resolved entry file lives inside the package; the nearest package.json
// above it is the compiler's own. Vue 3 does not export "./package.json".
function compilerMajor(resolvedEntry) {
  let dir = path.dirname(resolvedEntry);
  while (true) {
    try {
      const pkg = JSON.parse(readFileSync(path.join(dir, "package.json"), "utf8"));
      if (pkg?.name === "@vue/compiler-sfc") {
        const major = parseInt(String(pkg.version ?? "").split(".")[0], 10);
        return Number.isFinite(major) ? major : 0;
      }
    } catch {}
    const parent = path.dirname(dir);
    if (parent === dir) return 0;
    dir = parent;
  }
}

async function loadVueCompiler(startFilePath) {
  for (const dir of candidateResolveDirs(startFilePath)) {
    const req = createRequire(path.join(dir, "__grepai_vue_processor__.cjs"));
    try {
      const resolved = req.resolve("@vue/compiler-sfc");
      const mod = await import(pathToFileURL(resolved).href);
      return { mod, major: compilerMajor(resolved) };
    } catch (err) {
      if (err?.code !== "MODULE_NOT_FOUND") {
        throw err;
      }
    }
  }
  throw new Error(`Cannot resolve @vue/compiler-sfc from ${path.dirname(startFilePath)}`);
}

function errorText(err) {
  if (typeof err === "string") return err;
  if (err?.message) return String(err.message);
  // Vue 2 compiler errors are {msg, start, end}.
  if (err?.msg) return String(err.msg);
  try {
    return JSON.stringify(err);
  } catch {
    return String(err);
  }
}

function isDescriptor(value) {
  return Boolean(value) && typeof value === "object" && "styles" in value && !("descriptor" in value);
}

function hasBlocks(d) {
  return Boolean(d?.script || d?.scriptSetup || d?.template || d?.styles?.length);
}

// Vue 3: parse(source, options) -> { descriptor, errors }.
// Vue 2.7: parse(options) -> SFCDescriptor (errors on the descriptor). Passing
// the source string as the options object does not throw there; it returns a
// descriptor whose blocks are all null.
function parseDescriptor(parse, major, source, filename) {
  if (major >= 3) {
    const parsed = parse(source, { filename });
    return { descriptor: parsed?.descriptor, errors: parsed?.errors ?? [] };
  }
  if (major === 2) {
    const d = parse({ source, filename });
    return { descriptor: d, errors: d?.errors ?? [] };
  }
  const parsed = parse(source, { filename });
  if (parsed && typeof parsed === "object" && "descriptor" in parsed) {
    return { descriptor: parsed.descriptor, errors: parsed.errors ?? [] };
  }
  if (isDescriptor(parsed) && !hasBlocks(parsed)) {
    const d = parse({ source, filename });
    return { descriptor: d, errors: d?.errors ?? [] };
  }
  return { descriptor: parsed, errors: parsed?.errors ?? [] };
}

function lineOfOffset(text, offset) {
  let line = 1;
  const end = Math.min(offset, text.length);
  for (let i = 0; i < end; i++) if (text.charCodeAt(i) === 10) line++;
  return line;
}

// Vue 3 blocks carry loc.start.{line,offset}; Vue 2 blocks carry start/end
// offsets. The first content line sits on the line of the content offset.
function blockStartLine(block) {
  const offset = block?.loc?.start?.offset ?? block?.start;
  if (typeof offset === "number" && offset >= 0) return lineOfOffset(source, offset);
  const line = block?.loc?.start?.line;
  return typeof line === "number" && line > 0 ? line : 1;
}

function countLines(text) {
  if (text.length === 0) return 1;
  return text.split("\n").length;
}

function pushMapped(out, map, text, sourceStartLine) {
  if (!text) return;
  out.push(text);
  const lineCount = countLines(text);
  for (let i = 0; i < lineCount; i++) {
    map.push(sourceStartLine + i);
  }
}

function pushUnmapped(out, map, text) {
  if (!text) return;
  out.push(text);
  const lineCount = countLines(text);
  for (let i = 0; i < lineCount; i++) {
    map.push(0);
  }
}

function normalizeExpr(expr) {
  const trimmed = expr.trim();
  const singleQuoted = /^'([^']+)'$/.exec(trimmed);
  if (singleQuoted) return singleQuoted[1];
  const doubleQuoted = /^"([^"]+)"$/.exec(trimmed);
  if (doubleQuoted) return doubleQuoted[1];
  return trimmed;
}

function extractVBindExprsFromLine(line) {
  const out = [];
  let start = 0;
  while (start < line.length) {
    const idx = line.indexOf("v-bind(", start);
    if (idx < 0) break;
    let i = idx + "v-bind(".length;
    let depth = 1;
    while (i < line.length && depth > 0) {
      const ch = line[i];
      if (ch === "(") depth++;
      else if (ch === ")") depth--;
      i++;
    }
    if (depth === 0) {
      const expr = line.slice(idx + "v-bind(".length, i - 1);
      out.push(normalizeExpr(expr));
      start = i;
    } else {
      break;
    }
  }
  return out;
}

function extractStyleVBindExpressions(styleContent, styleStartLine) {
  const refs = [];
  const lines = styleContent.split("\n");
  for (let i = 0; i < lines.length; i++) {
    const exprs = extractVBindExprsFromLine(lines[i]);
    for (const expr of exprs) {
      refs.push({
        expr,
        line: styleStartLine + i,
      });
    }
  }
  return refs;
}

function appendStyleBindings(out, map, refs, index) {
  if (!refs.length) return;
  out.push(`function __vue_style_bindings__${index}() {`);
  map.push(refs[0].line);
  for (const ref of refs) {
    out.push(`  __css_v_bind__(${ref.expr});`);
    map.push(ref.line);
  }
  out.push("}");
  map.push(refs[refs.length - 1].line);
}

try {
  const { mod, major } = await loadVueCompiler(absoluteFilePath);
  const { parse, compileTemplate } = mod;
  if (typeof parse !== "function") {
    throw new Error(`@vue/compiler-sfc resolved for ${filePath} has no parse()`);
  }
  const { descriptor: d, errors } = parseDescriptor(parse, major, source, filePath);
  if (!d || typeof d !== "object") {
    throw new Error(`@vue/compiler-sfc ${major || "?"}.x returned no descriptor for ${filePath}`);
  }
  // Vue 3 parse() reports template expression errors here while still
  // returning the script and style blocks. Keep what the compiler gave us;
  // only a descriptor with nothing in it goes to the regex fallback.
  if (errors?.length && !hasBlocks(d)) {
    throw new Error(errorText(errors[0]));
  }

  const out = [];
  const map = [];
  const warnings = new Set();
  const parseMessage = errors?.length ? errorText(errors[0]) : "";
  if (parseMessage) {
    warnings.add(`vue parse error in ${filePath}: ${parseMessage}`);
  }

  // A block boundary is exactly one generated line, mapped to no source line.
  function separator() {
    if (out.length > 0) {
      out.push("");
      map.push(0);
    }
  }

  if (d.script?.content) {
    pushMapped(out, map, d.script.content, blockStartLine(d.script));
  }
  if (d.scriptSetup?.content) {
    separator();
    pushMapped(out, map, d.scriptSetup.content, blockStartLine(d.scriptSetup));
  }

  // A template the resolved compiler cannot handle (Vue 2 syntax under a Vue 3
  // compiler, or a real error) costs this file its template reads, not its
  // script and style output, and never disables the processor for other files.
  if (d.template?.content && typeof compileTemplate === "function") {
    let compiled;
    try {
      compiled = compileTemplate({
        id: filePath,
        source: d.template.content,
        filename: filePath,
        scoped: Boolean(d.styles?.some((s) => s.scoped)),
      });
    } catch (err) {
      compiled = { errors: [err] };
    }
    if (compiled?.errors?.length) {
      const message = errorText(compiled.errors[0]);
      // Vue 3 reports the same template error from parse(); one line is enough.
      if (message === parseMessage) warnings.delete(`vue parse error in ${filePath}: ${parseMessage}`);
      warnings.add(`vue template skipped for ${filePath}: ${message}`);
    } else if (compiled?.code) {
      separator();
      pushUnmapped(out, map, compiled.code);
    }
  }

  if (Array.isArray(d.styles) && d.styles.length > 0) {
    for (let i = 0; i < d.styles.length; i++) {
      const styleBlock = d.styles[i];
      if (!styleBlock?.content) continue;
      separator();
      const styleStartLine = blockStartLine(styleBlock);
      // Include raw style content for semantic search.
      pushMapped(out, map, styleBlock.content, styleStartLine);

      const refs = extractStyleVBindExpressions(styleBlock.content, styleStartLine);
      if (refs.length > 0) {
        separator();
        appendStyleBindings(out, map, refs, i);
      }
    }
  }

  const text = out.join("\n");
  process.stdout.write(
    JSON.stringify({
      embeddingText: text,
      traceText: text,
      virtualPath: `${filePath}.__trace__.ts`,
      generatedToSourceLine: map,
      warnings: [...warnings],
    }),
  );
} catch (err) {
  process.stderr.write(String(err?.stack || err?.message || err));
  process.exit(1);
}
