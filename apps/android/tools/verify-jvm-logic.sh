#!/usr/bin/env bash
#
# Compile and run the Android-free half of :relay-bridge on a plain JVM.
#
# WHY THIS EXISTS
#
# The real build is `./gradlew :relay-bridge:testDebugUnitTest`, and that is the
# command that matters. It needs the Android Gradle Plugin, which is published
# only on dl.google.com — so on a machine without an Android SDK, or behind an
# egress policy that blocks Google's Maven, nothing in this module can be
# compiled at all and every line of Kotlin here is unverified prose.
#
# Most of the interesting logic does not actually need Android. The protocol
# codec, the command catalog, the capture state machines, the storage policy,
# the audio ring, the OEM watchdog, the consent rules and the store-and-forward
# queue are plain Kotlin. This script compiles exactly the files that import no
# `android.*` symbol, together with their JUnit tests, using kotlinc from Maven
# Central, and runs them.
#
# It is a floor, not a substitute: it cannot see a manifest error, a Compose
# mistake, a resource that does not exist, or anything in `src/vendor/`. What it
# does prove is that the logic compiles and its tests pass. Treat a green run
# here as "the part that can be checked, was".
#
#   ./tools/verify-jvm-logic.sh
#
# Downloads ~65 MB of compiler on first run into .verify-cache/ (gitignored).

set -euo pipefail

MODULE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BRIDGE="$MODULE_ROOT/relay-bridge"
CACHE="${VERIFY_CACHE:-$MODULE_ROOT/.verify-cache}"
OUT="$CACHE/classes"
MAVEN="${MAVEN_CENTRAL:-https://repo1.maven.org/maven2}"

KOTLIN_VERSION="2.1.20"
COROUTINES_VERSION="1.8.1"

mkdir -p "$CACHE"

fetch() { # path-under-maven
  local path="$1" file
  file="$CACHE/$(basename "$path")"
  if [ ! -f "$file" ]; then
    # stderr, not stdout: this function is called inside $( ), so a progress line
    # on stdout becomes part of the path and every classpath entry is garbage.
    # The failure only appears on a cold cache, because a warm one skips the echo
    # — which is exactly the run nobody does before trusting the output.
    echo "  fetching $(basename "$path")" >&2
    curl -fsS -o "$file" "$MAVEN/$path"
  fi
  printf '%s' "$file"
}

echo "resolving toolchain"
COMPILER=$(fetch "org/jetbrains/kotlin/kotlin-compiler-embeddable/$KOTLIN_VERSION/kotlin-compiler-embeddable-$KOTLIN_VERSION.jar")
STDLIB=$(fetch "org/jetbrains/kotlin/kotlin-stdlib/$KOTLIN_VERSION/kotlin-stdlib-$KOTLIN_VERSION.jar")
REFLECT=$(fetch "org/jetbrains/kotlin/kotlin-reflect/$KOTLIN_VERSION/kotlin-reflect-$KOTLIN_VERSION.jar")
SCRIPTRT=$(fetch "org/jetbrains/kotlin/kotlin-script-runtime/$KOTLIN_VERSION/kotlin-script-runtime-$KOTLIN_VERSION.jar")
COROUTINES=$(fetch "org/jetbrains/kotlinx/kotlinx-coroutines-core-jvm/$COROUTINES_VERSION/kotlinx-coroutines-core-jvm-$COROUTINES_VERSION.jar")
COROUTINES_TEST=$(fetch "org/jetbrains/kotlinx/kotlinx-coroutines-test-jvm/$COROUTINES_VERSION/kotlinx-coroutines-test-jvm-$COROUTINES_VERSION.jar")
JUNIT=$(fetch "junit/junit/4.13.2/junit-4.13.2.jar")
HAMCREST=$(fetch "org/hamcrest/hamcrest-core/1.3/hamcrest-core-1.3.jar")
ANNOTATIONS=$(fetch "org/jetbrains/annotations/13.0/annotations-13.0.jar")
TROVE=$(fetch "org/jetbrains/intellij/deps/trove4j/1.0.20200330/trove4j-1.0.20200330.jar")
# org.json ships inside the Android platform, so the module never declares it.
# On a plain JVM it has to come from somewhere.
JSON=$(fetch "org/json/json/20240303/json-20240303.jar")

COMPILER_CP="$COMPILER:$STDLIB:$REFLECT:$SCRIPTRT:$ANNOTATIONS:$COROUTINES:$TROVE"
LIB_CP="$STDLIB:$COROUTINES:$COROUTINES_TEST:$JUNIT:$HAMCREST:$JSON"

# The Android-free set, by a rule rather than by a list someone has to remember
# to update. Two conditions, both mechanical:
#
#   1. the file lives in a *sub*package of glass.relay.bridge. The root package
#      is the Android glue — service, receiver, notification, transport factory —
#      and everything under it is deliberately platform-free logic.
#   2. it names no `android.*` symbol.
#
# Rule 2 alone is not enough: a file can import nothing from Android and still
# reference a class that does, and the compiler will rightly refuse.
# Read into the array with a loop rather than `mapfile`, which is a bash 4
# builtin. macOS still ships bash 3.2 as /bin/bash — Apple froze it at the last
# GPLv2 release — so `mapfile` is not there, and on the author's Mac this script
# aborted on this line having compiled and run nothing. It aborted honestly
# (`set -euo pipefail` is above), but a harness that cannot run on the machine
# holding the hardware is a harness that does not run where it matters most.
SOURCES=()
while IFS= read -r source; do
  SOURCES+=("$source")
done < <(
  find "$BRIDGE/src/main/java/glass/relay/bridge" "$BRIDGE/src/test/java/glass/relay/bridge" \
    -mindepth 2 -name '*.kt' -print0 |
    xargs -0 grep -L -E '(^import |[^a-zA-Z.])android[.x]' |
    sort
)

if [ "${#SOURCES[@]}" -eq 0 ]; then
  echo "no Android-free sources found — has the layout moved?" >&2
  exit 1
fi

echo "compiling ${#SOURCES[@]} Android-free files"
rm -rf "$OUT"
mkdir -p "$OUT"
java -cp "$COMPILER_CP" org.jetbrains.kotlin.cli.jvm.K2JVMCompiler \
  -nowarn -no-stdlib -no-reflect -jvm-target 17 \
  -cp "$LIB_CP" -d "$OUT" "${SOURCES[@]}"

# Appended directly rather than read back out of a process substitution.
#
# bash 3.2 — which is what macOS ships as /bin/bash — counts parentheses
# lexically when it parses `< <( … )`, and a `case` pattern's closing one ends
# the substitution early. There is no spelling of that combination which parses
# on both 3.2 and 4; this loop needs neither construct, so it sidesteps the
# question instead of encoding a workaround somebody would later "tidy up".
TEST_CLASSES=()
for source in "${SOURCES[@]}"; do
  case "$source" in
    */src/test/java/*) ;;
    *) continue ;;
  esac
  package=$(grep -m1 '^package ' "$source" | awk '{print $2}')
  base=$(basename "$source" .kt)
  if [ -f "$OUT/${package//.//}/$base.class" ]; then
    TEST_CLASSES+=("$package.$base")
  fi
done

if [ "${#TEST_CLASSES[@]}" -eq 0 ]; then
  echo "compiled, but found no test classes to run" >&2
  exit 1
fi

echo "running ${#TEST_CLASSES[@]} test classes"
java -cp "$OUT:$LIB_CP" org.junit.runner.JUnitCore "${TEST_CLASSES[@]}"
