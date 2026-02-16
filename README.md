Replacement for [kube-httpcache](https://github.com/mittwald/kube-httpcache).

Features:
- Supports startup/readiness probes without waiting for a non-empty frontend EndpointSlice (does not suffer from https://github.com/mittwald/kube-httpcache/issues/36 and https://github.com/mittwald/kube-httpcache/issues/222)
- Nicer CLI interface. varnishd cmd args can be directly specified after '--'
- Watching for changes of the VCL template in file system and dynamically reloading it at runtime
- Rollback for failed VCL template updates, such that frontend/backend changes still load fine after a failed VCL template was loaded
- Supports multiple backend groups
- Uses "<< ... >>" as Template delimiter to not clash with Helm templating
