# Exercises Starlark builtins added in the "Close several Starlark API
# compatibility gaps with Bazel" changes: attr.int_list(),
# native.repo_name(), native.repository_name(), native.module_name(),
# native.module_version(), native.package_name(), Label.workspace_name,
# native.subpackages(), native.existing_rule(),
# native.existing_rules(), and analysis_test_transition().

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")

def _info_file_impl(ctx):
    out = ctx.actions.declare_file(ctx.label.name + ".txt")
    total = 0
    for n in ctx.attr.numbers:
        total += n
    ctx.actions.write(
        output = out,
        content = "%s\nsum(%s) = %d\n" % (ctx.attr.build_info, ctx.attr.numbers, total),
    )
    return [DefaultInfo(files = depset([out]))]

info_file = rule(
    implementation = _info_file_impl,
    attrs = {
        "build_info": attr.string(),
        # attr.int_list() is one of the newly added builtins.
        "numbers": attr.int_list(default = [1, 2, 3]),
    },
)

def repo_info(name, **kwargs):
    # All of the functions below run at macro expansion time and are
    # newly added builtins.
    info_file(
        name = name,
        build_info = "module=%s version=%s repo=%r repository_name=%r package=%r workspace_name=%r subpackages=%r" % (
            native.module_name(),
            native.module_version(),
            native.repo_name(),
            native.repository_name(),
            native.package_name(),
            Label("//:BUILD.bazel").workspace_name,
            native.subpackages(include = ["**"]),
        ),
        **kwargs
    )

def verify_native_apis():
    # Loading phase assertions against native.subpackages(),
    # native.existing_rule() and native.existing_rules(). Failures
    # cause the package to fail to load, which fails the build.
    subpackages = native.subpackages(include = ["**"])
    if subpackages != ["platforms"]:
        fail("native.subpackages() returned %r, expected [\"platforms\"]" % subpackages)
    if native.subpackages(include = ["**"], exclude = ["plat*"], allow_empty = True) != []:
        fail("native.subpackages() with exclusions should have returned nothing")

    hello = native.existing_rule("hello")
    if not hello:
        fail("native.existing_rule(\"hello\") returned nothing")
    if hello["kind"] != "genrule":
        fail("native.existing_rule(\"hello\") has kind %r, expected \"genrule\"" % hello["kind"])
    if hello["name"] != "hello":
        fail("native.existing_rule(\"hello\") has name %r" % hello["name"])
    if native.existing_rule("does_not_exist") != None:
        fail("native.existing_rule() of a nonexistent target should return None")

    existing_rules = native.existing_rules()
    if "hello" not in existing_rules or "repo_info" not in existing_rules:
        fail("native.existing_rules() returned %r" % existing_rules.keys())
    if existing_rules["repo_info"]["kind"] != "info_file":
        fail("repo_info has kind %r, expected \"info_file\"" % existing_rules["repo_info"]["kind"])
    if existing_rules["repo_info"]["numbers"] != [4, 5, 6]:
        fail("repo_info has numbers %r, expected [4, 5, 6]" % existing_rules["repo_info"]["numbers"])

# analysis_test_transition() applies a constant change to build
# settings on the attribute's dependencies.
_mode_transition = analysis_test_transition(
    settings = {
        "//:mode": "transitioned",
    },
)

def _setting_reader_impl(ctx):
    # Attributes carrying a user-defined transition are exposed as a
    # list of configured targets, one per output configuration.
    setting = ctx.attr.setting
    if type(setting) == type([]):
        setting = setting[0]
    out = ctx.actions.declare_file(ctx.label.name + ".txt")
    ctx.actions.write(
        output = out,
        content = "mode=%s\n" % setting[BuildSettingInfo].value,
    )
    return [DefaultInfo(files = depset([out]))]

setting_reader = rule(
    implementation = _setting_reader_impl,
    attrs = {
        "setting": attr.label(cfg = _mode_transition),
    },
)
