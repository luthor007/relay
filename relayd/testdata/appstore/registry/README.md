# A registry, on disk

This is what `github.com/luthor007/relay-apps` is: a directory of manifests
pointing at source repositories (APP-PLATFORM.md §6). No build service, no
publishing pipeline, no API. Adding an app is one added file, so the review is
a pull request and the diff *is* the permission sheet.

```
index.json                          {"apps": [...]}
apps/<app-id>.json                  one entry: manifest + where the code is
```

`../fork/` is the same layout under a different name, and the tests resolve
from both to demonstrate that forking is a config change (`--registry`,
`RELAY_APP_REGISTRY`, or `app-registry` in the config directory) rather than a
patch to the client.

Everything here is synthetic. `dev.alexis.standup-notes` is byte-identical to
the manifest in `apps/sdk/examples/standup-notes/relay.json`, which is what
`TestTheSDKExampleInstalls` checks — the example app the SDK ships has to be an
app the box will actually take.
