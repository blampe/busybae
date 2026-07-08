# 🐝 busybae

busybae[zel] is a background daemon for local Bazel cache cleanup.

busybae watches Bazel's non-native-GC cache directories (`repository_cache`,
`disk_cache`, `repo_contents_cache`) and periodically evicts entries older than
a configurable age. It's designed to be invoked on every `bazel` command via a
[bazelisk](https://github.com/bazelbuild/bazelisk) wrapper: if a daemon isn't
running one will be started, otherwise it will no-op. The daemon exits after a
configurable idle period.

Cache directories are discovered by parsing the workspace's `.bazelrc`.

## Usage

```
busybae [poke]           Poke the daemon; start one if none is running.
busybae daemon           Run the daemon in the foreground.
busybae gc               One-shot sweep, no daemon.
busybae dirs             Print cache directories discovered from .bazelrc.
busybae install-wrapper  Write the bazelisk wrapper (tools/bazel) for this repo.
busybae version          Print the busybae version.
```

Run `busybae daemon -h` for the full flag list.

### Installing the bazelisk wrapper

The recommended integration is to check a `tools/bazel` script into your
workspace. [bazelisk](https://github.com/bazelbuild/bazelisk) automatically
execs `$WORKSPACE/tools/bazel` when present, so every `bazel` invocation
transparently pokes the daemon (or spawns one on first run) before running
the real build.

Generate the wrapper from a stamped busybae build:

```
bazel build --stamp //:busybae
./bazel-bin/busybae_/busybae install-wrapper
git add tools/bazel
```

The wrapper pins the busybae release version and per-platform SHA256
checksums, so fresh clones lazily materialize the binary under
`$XDG_CACHE_HOME/busybae/` with no bootstrap step. CI environments
(anything setting `$CI`) and users with `$BUSYBAE_DISABLE` set are passed
straight through to bazelisk.

To check for wrapper drift after a version bump (useful in CI):

```
busybae install-wrapper --check
```

### One-shot sweep

To run a single sweep without leaving a daemon behind:

```
busybae gc --max-age=168h
busybae gc --dry-run   # show what would be evicted
```

### Discovering cache directories

```
busybae dirs
```

Prints the `repository_cache`, `disk_cache`, `repo_contents_cache`, and
`output_user_root` that busybae resolved from your `.bazelrc`, along with
the effective max-age for each.

## Building

```
bazel build //:busybae
```
