"""Repository rule and module extension conformance checks.

The conformance_repo repository rule exercises the repository_ctx API
surface end to end: file I/O, templates, symlinks, path introspection,
command execution, environment access, downloading, and patching. Each
check appends a line to results.txt in the generated repository, which
the testproject's verify genrule asserts on. Failures during repo
evaluation fail the build outright.

The conformance module extension additionally checks the module_ctx
specific members before creating the repository.
"""

_LICENSE_URL = "https://raw.githubusercontent.com/bazelbuild/bazel-skylib/1.9.0/LICENSE"
_LICENSE_SHA256 = "cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30"
_TARBALL_URL = "https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz"
_TARBALL_SHA256 = "870c0fe1096223a58d4f8832d08a7e651ea2fcadb8e6877b2fdc26b662d481dd"

def _check(results, name, ok, detail = ""):
    results.append("{}={}{}".format(name, "ok" if ok else "FAIL", detail))

def _conformance_repo_impl(rctx):
    results = []

    # Simple context members.
    _check(results, "name", "conformance" in rctx.name)
    _check(results, "attr", rctx.attr.flavor == "vanilla")
    _check(results, "os_name", rctx.os.name == "linux")
    _check(results, "os_arch", rctx.os.arch == "amd64")
    _check(results, "os_environ_home", rctx.os.environ.get("HOME", "") != "")
    _check(results, "workspace_root", str(rctx.workspace_root) != "")

    # getenv() must be consistent with os.environ.
    _check(
        results,
        "getenv",
        rctx.getenv("HOME") == rctx.os.environ.get("HOME") and
        rctx.getenv("DOES_NOT_EXIST_42", "fallback") == "fallback",
    )

    # file() + read().
    rctx.file("hello.txt", "hello repo rules\n")
    _check(results, "file_read", rctx.read("hello.txt") == "hello repo rules\n")

    # file() with is_executable and legacy utf-8 content.
    rctx.file("tool.sh", "#!/bin/sh\necho tool-output\n", executable = True)

    # template() with substitutions.
    rctx.file("greeting.tpl", "greeting={GREETING} name={NAME}\n")
    rctx.template(
        "greeting.txt",
        "greeting.tpl",
        substitutions = {
            "{GREETING}": "hi",
            "{NAME}": "bonanza",
        },
    )
    _check(results, "template", rctx.read("greeting.txt") == "greeting=hi name=bonanza\n")

    # symlink() + path introspection.
    rctx.symlink("hello.txt", "hello_link.txt")
    _check(results, "symlink_read", rctx.read("hello_link.txt") == "hello repo rules\n")
    p = rctx.path("hello_link.txt")
    _check(results, "path_exists", p.exists)
    _check(results, "path_is_dir_file", not p.is_dir)
    rctx.file("subdir/nested.txt", "nested\n")
    d = rctx.path("subdir")
    _check(results, "path_is_dir_dir", d.is_dir)
    _check(results, "path_readdir", [f.basename for f in d.readdir()] == ["nested.txt"])
    _check(results, "path_basename", p.basename == "hello_link.txt")
    _check(results, "path_realpath", str(rctx.path("hello_link.txt").realpath).endswith("hello.txt"))

    # execute().
    result = rctx.execute(["/bin/sh", "-c", "echo exec-stdout; echo exec-stderr >&2; exit 7"])
    _check(
        results,
        "execute",
        result.return_code == 7 and
        result.stdout == "exec-stdout\n" and
        result.stderr == "exec-stderr\n",
    )

    # execute() with working_directory and environment.
    result = rctx.execute(
        ["/bin/sh", "-c", "echo $CONFORMANCE_VAR; pwd"],
        environment = {"CONFORMANCE_VAR": "var-value"},
        working_directory = "subdir",
    )
    _check(
        results,
        "execute_env_cwd",
        result.return_code == 0 and
        result.stdout.startswith("var-value\n") and
        result.stdout.rstrip().endswith("subdir"),
    )

    # which().
    _check(results, "which", str(rctx.which("sh")).endswith("sh"))

    # download() with integrity pinning.
    rctx.download(
        url = [_LICENSE_URL],
        output = "downloaded_license",
        sha256 = _LICENSE_SHA256,
    )
    _check(results, "download", "Apache License" in rctx.read("downloaded_license"))

    # download_and_extract() of a tarball with strip_prefix.
    rctx.download_and_extract(
        url = [_TARBALL_URL],
        output = "leftpad",
        sha256 = _TARBALL_SHA256,
        stripPrefix = "package",
    )
    _check(results, "download_and_extract", "leftPad" in rctx.read("leftpad/index.js"))

    # patch().
    rctx.file("patch_target.txt", "before\n")
    rctx.file("fix.patch", """--- a/patch_target.txt
+++ b/patch_target.txt
@@ -1 +1 @@
-before
+after
""")
    rctx.patch("fix.patch", strip = 1)
    _check(results, "patch", rctx.read("patch_target.txt") == "after\n")

    # delete().
    rctx.file("doomed.txt", "x")
    _check(results, "delete", rctx.delete("doomed.txt") and not rctx.path("doomed.txt").exists)

    # report_progress() must at least not fail.
    rctx.report_progress("conformance checks almost done")
    _check(results, "report_progress", True)

    failures = [r for r in results if "=FAIL" in r]
    if failures:
        fail("repository_ctx conformance failures:\n" + "\n".join(failures))

    rctx.file("results.txt", "\n".join(results) + "\n")
    rctx.file("BUILD.bazel", 'exports_files(["results.txt"], visibility = ["//visibility:public"])\n')

conformance_repo = repository_rule(
    implementation = _conformance_repo_impl,
    attrs = {
        "flavor": attr.string(default = "plain"),
    },
)

def _conformance_ext_impl(mctx):
    # module_ctx specific members.
    if not mctx.root_module_has_non_dev_dependency:
        fail("root_module_has_non_dev_dependency should be True")
    seen_root = False
    for module in mctx.modules:
        if module.is_root:
            seen_root = True
            if module.name != "testproject":
                fail("unexpected root module name %s" % module.name)
            for tag in module.tags.flavor:
                if not mctx.is_dev_dependency(tag) and tag.name != "vanilla":
                    fail("unexpected non-dev flavor tag %s" % tag.name)
    if not seen_root:
        fail("root module not visible in module_ctx.modules")

    conformance_repo(
        name = "conformance",
        flavor = "vanilla",
    )

conformance_ext = module_extension(
    implementation = _conformance_ext_impl,
    tag_classes = {
        "flavor": tag_class(attrs = {"name": attr.string()}),
    },
)
