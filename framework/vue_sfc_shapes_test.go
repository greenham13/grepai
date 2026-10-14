package framework

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fixture shared by every shape test: script, Vue 2 filter template, scoped
// style with a v-bind. Line numbers below refer to this text.
const vue2FilterSFC = `<template>
  <div class="card">
    <span>{{ price | currency }}</span>
    <button @click="buy(item)">{{ label }}</button>
  </div>
</template>

<script>
export function buy(item) {
  return item.id
}
export default {
  name: 'Vue2Filter',
}
</script>

<style scoped>
.card { color: v-bind(color); }
</style>
`

const vue3SetupSFC = `<template>
  <div class="card">{{ formatted(price) }}</div>
</template>

<script lang="ts">
export function helper(n: number): number {
  return n * 2
}
</script>

<script setup lang="ts">
const price = 1
function formatted(v: number) { return '$' + helper(v) }
</script>
`

// fakeVue27Compiler mirrors @vue/compiler-sfc 2.7.16 as probed on 2026-10-13:
// parse(options) returns the descriptor itself, blocks carry start/end
// offsets, parse(string) returns a descriptor with every block null instead
// of throwing, compileTemplate reads `_vm.*` and reports {msg,start,end}.
// Like 2.7.16 it defaults prettify to true; a hoisted prettier 3 then makes
// `code` a Promise, which is what this fake returns unless prettify is off.
const fakeVue27Compiler = `
function block(source, tag) {
  const re = new RegExp("<" + tag + "[^>]*>([\\s\\S]*?)</" + tag + ">");
  const m = re.exec(source);
  if (!m) return null;
  const start = m.index + m[0].indexOf(">") + 1;
  return { type: tag, content: m[1], start, end: start + m[1].length, attrs: {}, scoped: m[0].includes("scoped") };
}
function empty() {
  return { source: "", filename: "", template: null, script: null, scriptSetup: null, styles: [], customBlocks: [], cssVars: [], errors: [], shouldForceReload: () => false };
}
export function parse(options) {
  if (typeof options !== "object" || options === null) return empty();
  const s = options.source;
  const style = block(s, "style");
  return { source: s, filename: options.filename, template: block(s, "template"), script: block(s, "script"), scriptSetup: null,
    styles: style ? [style] : [], customBlocks: [], cssVars: [], errors: [], shouldForceReload: () => false };
}
export function compileTemplate(options) {
  if (options.source.includes("BREAK")) {
    return { ast: null, code: "", source: options.source, tips: [], errors: [{ msg: "invalid expression: BREAK", start: 0, end: 5 }] };
  }
  const code = "var render = function render() {\n  var _vm = this,\n    _c = _vm._self._c\n  return _c('span', [_vm._v(_vm._s(_vm._f('currency')(_vm.price))), _vm.buy(_vm.item)])\n}";
  return { ast: null, source: options.source, tips: [], errors: [],
    code: options.prettify === false ? code : Promise.resolve(code) };
}
`

// fakeVue3Compiler mirrors @vue/compiler-sfc 3.5.44: parse(source, options)
// returns {descriptor, errors}, blocks carry loc.start.{line,column,offset},
// parse(object) throws, template expression errors are reported by parse()
// as well as by compileTemplate(), and render code reads `_ctx.*`.
const fakeVue3Compiler = `
function lineOf(source, offset) { let line = 1; for (let i = 0; i < offset; i++) if (source[i] === "\n") line++; return line; }
function block(source, tag) {
  const re = new RegExp("<" + tag + "[^>]*>([\\s\\S]*?)</" + tag + ">", "g");
  const out = [];
  let m;
  while ((m = re.exec(source))) {
    const offset = m.index + m[0].indexOf(">") + 1;
    out.push({ type: tag, content: m[1], attrs: {}, scoped: m[0].includes("scoped"), setup: m[0].includes("setup"),
      loc: { start: { line: lineOf(source, offset), column: 1, offset }, end: { line: 0, column: 0, offset: offset + m[1].length }, source: m[1] } });
  }
  return out;
}
export function parse(source, options) {
  if (typeof source !== "string") throw new TypeError("currentInput.slice is not a function");
  const scripts = block(source, "script");
  const template = block(source, "template")[0] ?? null;
  const errors = [];
  if (template && template.content.includes("BREAK")) errors.push(new SyntaxError('Error parsing JavaScript expression: Unexpected token, expected "," (1:17)'));
  return { descriptor: { filename: options?.filename, source, template, script: scripts.find(s => !s.setup) ?? null,
    scriptSetup: scripts.find(s => s.setup) ?? null, styles: block(source, "style"), customBlocks: [], cssVars: [] }, errors };
}
export function compileTemplate(options) {
  if (options.source.includes("BREAK")) {
    return { code: "", ast: null, preamble: "", source: options.source, tips: [], map: undefined,
      errors: [new SyntaxError('Error parsing JavaScript expression: Unexpected token, expected "," (1:17)')] };
  }
  return { ast: null, preamble: "", source: options.source, tips: [], errors: [], map: undefined,
    code: "export function render(_ctx, _cache) {\n  return _toDisplayString(_ctx.formatted(_ctx.price)) + _ctx.buy(_ctx.item)\n}" };
}
`

func writeFakeCompiler(t *testing.T, projectDir, version, module string) {
	t.Helper()
	dir := filepath.Join(projectDir, "node_modules", "@vue", "compiler-sfc")
	if err := os.MkdirAll(filepath.Join(dir, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	pkg := `{"name":"@vue/compiler-sfc","version":"` + version + `","type":"module","exports":{".":"./dist/index.mjs"}}`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dist", "index.mjs"), []byte(module), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeComponent writes src/<name> under projectDir and returns the
// project-relative path, which is what the indexer hands to processors.
func writeComponent(t *testing.T, projectDir, name, source string) string {
	t.Helper()
	rel := filepath.Join("src", name)
	file := filepath.Join(projectDir, rel)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	return rel
}

// sourceLineOf returns the source line the generated line holding `needle`
// maps to, and fails when the generated text does not contain it.
func sourceLineOf(t *testing.T, res TransformResult, needle string) int {
	t.Helper()
	for i, line := range strings.Split(res.Text, "\n") {
		if strings.Contains(line, needle) {
			return RemapLine(res.GeneratedToSourceLine, i+1)
		}
	}
	t.Fatalf("generated text has no line containing %q:\n%s", needle, res.Text)
	return 0
}

func lineInSource(t *testing.T, source, needle string) int {
	t.Helper()
	for i, line := range strings.Split(source, "\n") {
		if strings.Contains(line, needle) {
			return i + 1
		}
	}
	t.Fatalf("fixture has no line containing %q", needle)
	return 0
}

func assertScriptAndTemplate(t *testing.T, res TransformResult, source string, reads ...string) {
	t.Helper()
	for _, want := range []string{"export function buy(item)", "function __vue_template_reads__() {", "__css_v_bind__(color);"} {
		if !strings.Contains(res.Text, want) {
			t.Fatalf("missing %q in:\n%s", want, res.Text)
		}
	}
	for _, name := range reads {
		if !strings.Contains(res.Text, "\n  "+name+"();") {
			t.Fatalf("template read %q missing in:\n%s", name, res.Text)
		}
	}
	for _, internal := range []string{"_self();", "_v();", "_s();", "_f();"} {
		if strings.Contains(res.Text, internal) {
			t.Fatalf("internal render helper leaked as a read %q:\n%s", internal, res.Text)
		}
	}
	if len(res.GeneratedToSourceLine) != len(strings.Split(res.Text, "\n")) {
		t.Fatalf("line map has %d entries for %d generated lines", len(res.GeneratedToSourceLine), len(strings.Split(res.Text, "\n")))
	}
	for _, needle := range []string{"export function buy(item)", "return item.id", "name: 'Vue2Filter'", ".card { color: v-bind(color); }"} {
		if got, want := sourceLineOf(t, res, needle), lineInSource(t, source, needle); got != want {
			t.Fatalf("%q maps to source line %d, want %d", needle, got, want)
		}
	}
}

// assertRelativeIdentity checks that the index identity of the file stays the
// project-relative path the indexer passed in, whatever the resolution path.
func assertRelativeIdentity(t *testing.T, res TransformResult, rel string) {
	t.Helper()
	if res.FilePath != rel {
		t.Fatalf("FilePath %q, want the relative path %q", res.FilePath, rel)
	}
	if res.VirtualPath != rel+".__trace__.ts" {
		t.Fatalf("VirtualPath %q, want %q", res.VirtualPath, rel+".__trace__.ts")
	}
}

func TestVueProcessorTransform_Vue27CompilerShape(t *testing.T) {
	project := filepath.Join(t.TempDir(), "vue2")
	writeFakeCompiler(t, project, "2.7.16", fakeVue27Compiler)
	rel := writeComponent(t, project, "Card.vue", vue2FilterSFC)

	res, err := NewVueProcessor("node", project).TransformForEmbedding(context.Background(), rel, vue2FilterSFC)
	if err != nil {
		t.Fatalf("transform failed: %v", err)
	}
	if !strings.Contains(res.Text, "var render = function render()") {
		t.Fatalf("Vue 2 render code missing (prettify left on?):\n%s", res.Text)
	}
	assertScriptAndTemplate(t, res, vue2FilterSFC, "buy", "item", "price")
	assertRelativeIdentity(t, res, rel)
	if len(res.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", res.Warnings)
	}
}

func TestVueProcessorTransform_Vue3CompilerShape(t *testing.T) {
	project := filepath.Join(t.TempDir(), "vue3")
	writeFakeCompiler(t, project, "3.5.44", fakeVue3Compiler)
	rel := writeComponent(t, project, "Card.vue", vue2FilterSFC)

	res, err := NewVueProcessor("node", project).TransformForEmbedding(context.Background(), rel, vue2FilterSFC)
	if err != nil {
		t.Fatalf("transform failed: %v", err)
	}
	if !strings.Contains(res.Text, "export function render(_ctx, _cache)") {
		t.Fatalf("Vue 3 render code missing:\n%s", res.Text)
	}
	assertScriptAndTemplate(t, res, vue2FilterSFC, "buy", "item", "price")
	assertRelativeIdentity(t, res, rel)

	setup := writeComponent(t, project, "Setup.vue", vue3SetupSFC)
	res, err = NewVueProcessor("node", project).TransformForEmbedding(context.Background(), setup, vue3SetupSFC)
	if err != nil {
		t.Fatalf("transform failed: %v", err)
	}
	for _, needle := range []string{"export function helper(n: number)", "return n * 2", "const price = 1", "function formatted(v: number)"} {
		if got, want := sourceLineOf(t, res, needle), lineInSource(t, vue3SetupSFC, needle); got != want {
			t.Fatalf("%q maps to source line %d, want %d", needle, got, want)
		}
	}
}

func TestVueProcessorTransform_UnknownCompilerVersionUsesReturnedShape(t *testing.T) {
	project := filepath.Join(t.TempDir(), "unknown")
	writeFakeCompiler(t, project, "", fakeVue27Compiler)
	rel := writeComponent(t, project, "Card.vue", vue2FilterSFC)

	res, err := NewVueProcessor("node", project).TransformForEmbedding(context.Background(), rel, vue2FilterSFC)
	if err != nil {
		t.Fatalf("transform failed: %v", err)
	}
	assertScriptAndTemplate(t, res, vue2FilterSFC, "buy", "item", "price")
}

// A workspace watcher serves every project from one process whose cwd is the
// workspace folder. Relative paths must resolve against the project root the
// processor was built for, never against cwd.
func TestVueProcessorTransform_RelativePathResolvesAgainstRootNotCwd(t *testing.T) {
	root := t.TempDir()
	vue2 := filepath.Join(root, "vue2app")
	vue3 := filepath.Join(root, "vue3app")
	writeFakeCompiler(t, vue2, "2.7.16", fakeVue27Compiler)
	writeFakeCompiler(t, vue3, "3.5.44", fakeVue3Compiler)
	rel := writeComponent(t, vue2, "Card.vue", vue2FilterSFC)
	writeComponent(t, vue3, "Card.vue", vue2FilterSFC)

	for _, cwd := range []string{root, vue3} {
		t.Run(filepath.Base(cwd), func(t *testing.T) {
			t.Chdir(cwd)
			res, err := NewVueProcessor("node", vue2).TransformForEmbedding(context.Background(), rel, vue2FilterSFC)
			if err != nil {
				t.Fatalf("transform failed with cwd %s: %v", cwd, err)
			}
			if !strings.Contains(res.Text, "var render = function render()") {
				t.Fatalf("vue2app file compiled by the wrong compiler with cwd %s:\n%s", cwd, res.Text)
			}
			assertRelativeIdentity(t, res, rel)
			if len(res.Warnings) != 0 {
				t.Fatalf("unexpected warnings: %v", res.Warnings)
			}
		})
	}

	// Without a root the processor keeps the old contract: cwd is the project.
	t.Chdir(vue3)
	res, err := NewVueProcessor("node", "").TransformForEmbedding(context.Background(), rel, vue2FilterSFC)
	if err != nil {
		t.Fatalf("transform failed: %v", err)
	}
	if !strings.Contains(res.Text, "export function render(_ctx, _cache)") {
		t.Fatalf("rootless processor should resolve against cwd:\n%s", res.Text)
	}
}

func TestVueProcessorTransform_TemplateErrorKeepsScriptAndStyle(t *testing.T) {
	broken := strings.Replace(vue2FilterSFC, "{{ label }}", "{{ label | BREAK 20 }}", 1)
	for _, tc := range []struct{ name, version, module, want string }{
		{"vue3", "3.5.44", fakeVue3Compiler, `Error parsing JavaScript expression: Unexpected token, expected "," (1:17)`},
		{"vue27", "2.7.16", fakeVue27Compiler, "invalid expression: BREAK"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := filepath.Join(t.TempDir(), tc.name)
			writeFakeCompiler(t, project, tc.version, tc.module)
			rel := writeComponent(t, project, "Broken.vue", broken)

			res, err := NewVueProcessor("node", project).TransformForEmbedding(context.Background(), rel, broken)
			if err != nil {
				t.Fatalf("a template error must not fail the file: %v", err)
			}
			if !strings.Contains(res.Text, "export function buy(item)") || !strings.Contains(res.Text, "__css_v_bind__(color);") {
				t.Fatalf("script or style output lost:\n%s", res.Text)
			}
			if strings.Contains(res.Text, "render") || strings.Contains(res.Text, "__vue_template_reads__") {
				t.Fatalf("template output should be skipped:\n%s", res.Text)
			}
			// The warning names the file by its index identity, not its disk path.
			if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "vue template skipped for "+rel+": "+tc.want) {
				t.Fatalf("want one template-skipped warning with %q, got %v", tc.want, res.Warnings)
			}
			if got, want := sourceLineOf(t, res, "return item.id"), lineInSource(t, broken, "return item.id"); got != want {
				t.Fatalf("script maps to %d, want %d", got, want)
			}
		})
	}
}

// compileTemplate under Vue 2.7 with prettify on and prettier 3 resolved
// returns a Promise for code; that must be a per-file warning, not a crash
// that sends the file to the regex fallback.
func TestVueProcessorTransform_NonStringTemplateCodeIsWarning(t *testing.T) {
	project := filepath.Join(t.TempDir(), "vue27-prettier3")
	const prettified = "options.prettify === false ? code : Promise.resolve(code)"
	if !strings.Contains(fakeVue27Compiler, prettified) {
		t.Fatal("fake compiler no longer models the prettier branch")
	}
	writeFakeCompiler(t, project, "2.7.16", strings.Replace(fakeVue27Compiler, prettified, "Promise.resolve(code)", 1))
	rel := writeComponent(t, project, "Card.vue", vue2FilterSFC)

	res, err := NewVueProcessor("node", project).TransformForEmbedding(context.Background(), rel, vue2FilterSFC)
	if err != nil {
		t.Fatalf("non-string template code must not fail the file: %v", err)
	}
	if !strings.Contains(res.Text, "export function buy(item)") || !strings.Contains(res.Text, "__css_v_bind__(color);") {
		t.Fatalf("script or style output lost:\n%s", res.Text)
	}
	if strings.Contains(res.Text, "render") {
		t.Fatalf("template output should be skipped:\n%s", res.Text)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "vue template skipped for "+rel+": compiler returned a Promise instead of code") {
		t.Fatalf("want one Promise warning, got %v", res.Warnings)
	}
}

func TestVueProcessorTransform_MissingCompilerScopedToDirectoryChain(t *testing.T) {
	root := t.TempDir()
	bare := filepath.Join(root, "bare")
	equipped := filepath.Join(root, "equipped")
	writeFakeCompiler(t, equipped, "2.7.16", fakeVue27Compiler)
	rel := writeComponent(t, bare, "A.vue", vue2FilterSFC)
	writeComponent(t, equipped, "A.vue", vue2FilterSFC)
	nested := filepath.Join(bare, "src", "nested")
	writeFakeCompiler(t, nested, "2.7.16", fakeVue27Compiler)
	nestedRel := writeComponent(t, nested, "C.vue", vue2FilterSFC)

	// One processor per project, as buildFrameworkRegistry does; the bare
	// project's marks must not leak into the equipped one even though both
	// see the same relative path.
	bareP := NewVueProcessor("node", bare)
	equippedP := NewVueProcessor("node", equipped)
	res, err := bareP.TransformForEmbedding(context.Background(), rel, vue2FilterSFC)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("bare project should report the compiler missing, got err=%v", err)
	}
	if len(res.Warnings) == 0 || !strings.Contains(res.Warnings[0], "Cannot resolve @vue/compiler-sfc from "+filepath.Join(bare, "src")) {
		t.Fatalf("missing-compiler warning naming the absolute directory expected, got %v", res.Warnings)
	}
	if _, ok := bareP.missingCompilerMessage(filepath.Join(bare, rel)); !ok {
		t.Fatal("bare project directory should be remembered as missing")
	}
	if msg, ok := bareP.missingCompilerMessage(filepath.Join(root, "x.vue")); !ok || !strings.Contains(msg, "from "+root+";") {
		t.Fatalf("ancestor directories were walked by resolution and must be remembered with their own path, got ok=%v msg=%q", ok, msg)
	}
	if _, ok := bareP.missingCompilerMessage(filepath.Join(nested, nestedRel)); ok {
		t.Fatal("a subdirectory with its own node_modules must not inherit the missing mark")
	}
	if _, ok := equippedP.missingCompilerMessage(filepath.Join(equipped, rel)); ok {
		t.Fatal("sibling project must not inherit the missing mark")
	}

	res, err = equippedP.TransformForEmbedding(context.Background(), rel, vue2FilterSFC)
	if err != nil {
		t.Fatalf("sibling project with a compiler must still compile: %v", err)
	}
	if !strings.Contains(res.Text, "var render = function render()") {
		t.Fatalf("sibling project lost compiled output:\n%s", res.Text)
	}
	res, err = NewVueProcessor("node", nested).TransformForEmbedding(context.Background(), nestedRel, vue2FilterSFC)
	if err != nil || !strings.Contains(res.Text, "var render = function render()") {
		t.Fatalf("nested project with its own compiler must compile: err=%v text=%q", err, res.Text)
	}

	res, err = bareP.TransformForEmbedding(context.Background(), filepath.Join("src", "D.vue"), vue2FilterSFC)
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(res.Text, "export function buy(item)") {
		t.Fatalf("bare project should keep using the fallback: err=%v", err)
	}
	if res.FilePath != filepath.Join("src", "D.vue") {
		t.Fatalf("fallback FilePath %q should stay relative", res.FilePath)
	}
}

func TestVueProcessorTransform_AbsolutePathWithoutRoot(t *testing.T) {
	project := filepath.Join(t.TempDir(), "vue2")
	writeFakeCompiler(t, project, "2.7.16", fakeVue27Compiler)
	abs := filepath.Join(project, writeComponent(t, project, "Card.vue", vue2FilterSFC))

	res, err := NewVueProcessor("node", "").TransformForEmbedding(context.Background(), abs, vue2FilterSFC)
	if err != nil {
		t.Fatalf("transform failed: %v", err)
	}
	assertScriptAndTemplate(t, res, vue2FilterSFC, "buy", "item", "price")
	if res.VirtualPath != abs+".__trace__.ts" {
		t.Fatalf("VirtualPath %q", res.VirtualPath)
	}
}

func TestVueProcessorFallback_LineMapExact(t *testing.T) {
	res, err := NewVueProcessor("node", "").fallback("Comp.vue", vue3SetupSFC+"<style scoped>\n.a { color: v-bind(tone); }\n</style>\n")
	if err != nil {
		t.Fatalf("fallback failed: %v", err)
	}
	if len(res.GeneratedToSourceLine) != len(strings.Split(res.Text, "\n")) {
		t.Fatalf("line map has %d entries for %d generated lines:\n%s", len(res.GeneratedToSourceLine), len(strings.Split(res.Text, "\n")), res.Text)
	}
	for _, needle := range []string{"export function helper(n: number)", "const price = 1", "function formatted(v: number)"} {
		if got, want := sourceLineOf(t, res, needle), lineInSource(t, vue3SetupSFC, needle); got != want {
			t.Fatalf("%q maps to source line %d, want %d", needle, got, want)
		}
	}
	if got := sourceLineOf(t, res, "__css_v_bind__(tone);"); got != 16 {
		t.Fatalf("style binding maps to %d, want 16", got)
	}
}

// Real compilers: GREPAI_VUE_COMPILER_DIRS lists project directories (colon
// separated) whose node_modules carry @vue/compiler-sfc; each one's
// node_modules is linked into a temp project and the fixture is compiled
// through a relative path while the process cwd is somewhere else.
func TestVueProcessorTransform_RealCompilers(t *testing.T) {
	dirs := os.Getenv("GREPAI_VUE_COMPILER_DIRS")
	if dirs == "" {
		t.Skip("set GREPAI_VUE_COMPILER_DIRS=<project>:<project> to run against installed @vue/compiler-sfc packages")
	}
	broken := strings.Replace(vue2FilterSFC, "{{ label }}", "{{ label | truncate 20 }}", 1)
	for _, dir := range strings.Split(dirs, ":") {
		pkg, err := os.ReadFile(filepath.Join(dir, "node_modules", "@vue", "compiler-sfc", "package.json"))
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		var meta struct{ Version string }
		if err := json.Unmarshal(pkg, &meta); err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		t.Run(filepath.Base(dir), func(t *testing.T) {
			workspace := t.TempDir()
			project := filepath.Join(workspace, "project")
			if err := os.MkdirAll(project, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(dir, "node_modules"), filepath.Join(project, "node_modules")); err != nil {
				t.Fatal(err)
			}
			t.Chdir(workspace)
			rel := writeComponent(t, project, "Card.vue", vue2FilterSFC)
			res, err := NewVueProcessor("node", project).TransformForEmbedding(context.Background(), rel, vue2FilterSFC)
			if err != nil {
				t.Fatalf("transform failed: %v", err)
			}
			assertScriptAndTemplate(t, res, vue2FilterSFC, "buy", "item", "price", "label")
			assertRelativeIdentity(t, res, rel)
			t.Logf("@vue/compiler-sfc %s: %d generated lines, warnings %v", meta.Version, len(strings.Split(res.Text, "\n")), res.Warnings)

			brokenRel := writeComponent(t, project, "Broken.vue", broken)
			res, err = NewVueProcessor("node", project).TransformForEmbedding(context.Background(), brokenRel, broken)
			if err != nil {
				t.Fatalf("broken template must not fail the file: %v", err)
			}
			if !strings.Contains(res.Text, "export function buy(item)") {
				t.Fatalf("script lost on broken template:\n%s", res.Text)
			}
			t.Logf("broken template warnings: %v", res.Warnings)
		})
	}
}
