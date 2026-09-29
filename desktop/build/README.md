# Wails build assets

`appicon.svg` and `trayicon.svg` are the editable icon sources. Their PNG counterparts and the Darwin property lists are consumed by local Wails v3 builds. The generated macOS resource is named `appicon.icns` so it does not reuse Wails' default icon cache entry. Generated application bundles are written to `build/bin/` and remain ignored.
