# A fork of the registry

Same layout, different owner, one app — a later version of
`dev.alexis.standup-notes` that asks for `glasses.camera` on top of what the
upstream entry asks for.

It is here to hold one property honest: **switching to a fork is a config
change, not a patch.** The tests resolve from `../registry` and from this
directory through the same client with nothing changed but a spec string, and
the extra scope makes the upgrade re-ask for consent rather than inherit it.
