# Changelog

## [0.2.0](https://github.com/HBTGmbH/k8s-httpcache/compare/v0.1.0...v0.2.0) (2026-02-25)


### Features

* add --debounce-max duration ([aa82e9a](https://github.com/HBTGmbH/k8s-httpcache/commit/aa82e9ad67eef391d7f6b16bc5475adb05db6a01))
* add /status endpoint on metrics listener/port ([2761bb9](https://github.com/HBTGmbH/k8s-httpcache/commit/2761bb9689dcd4f050ccb04c2bb34e08ce792773))
* add broadcast support and add more tests ([a3e74c4](https://github.com/HBTGmbH/k8s-httpcache/commit/a3e74c4b5600cd9aed90e92d0a452a8ebc2d25a8))
* add config to specify kept old VCL objects after reload ([8120e55](https://github.com/HBTGmbH/k8s-httpcache/commit/8120e559d91bda9b6b43bca155e9acbc7f59952e))
* add configurable VCL templating delimiters ([9e4033e](https://github.com/HBTGmbH/k8s-httpcache/commit/9e4033e555028ce6d2ab4ee8a5af64ffc40ce0e0))
* add emitting Kubernetes events and add more metrics ([c49f628](https://github.com/HBTGmbH/k8s-httpcache/commit/c49f62850da70b07fb898ba4e6577682f67edaf0))
* add log format config ([894a8b8](https://github.com/HBTGmbH/k8s-httpcache/commit/894a8b85a7451c5e2518250d98b86ab29fad3251))
* add Masterminds/sprig templating functions ([15e4965](https://github.com/HBTGmbH/k8s-httpcache/commit/15e49657244b55195b80de5258effe0d1725d5c9))
* add Prometheus metrics ([a49ffaf](https://github.com/HBTGmbH/k8s-httpcache/commit/a49ffafafa2fdcbd0ea7056bb1715280eba7a214))
* add separate backend/frontend debounce configs ([337298e](https://github.com/HBTGmbH/k8s-httpcache/commit/337298e50d0e6607a40f0bf39ed27fd9b398fa53))
* add support for --version ([b1e4fac](https://github.com/HBTGmbH/k8s-httpcache/commit/b1e4fac3f30e6a2a421cbe206ebe1c3029f19a89))
* add support for 'trunk' Varnish version ([16f0fa9](https://github.com/HBTGmbH/k8s-httpcache/commit/16f0fa99cc52413d650097f75bfa1229d34b2c55))
* add support for ExternalName backends ([64f0298](https://github.com/HBTGmbH/k8s-httpcache/commit/64f029844e4be373dc682c9e8205470c40ddfbd3))
* allow to specify log level and use log/slog ([a345e26](https://github.com/HBTGmbH/k8s-httpcache/commit/a345e2644ea2042fde12018c3bac75561584b1c3))
* emit error on ExternalName service with named port ([19a0ac6](https://github.com/HBTGmbH/k8s-httpcache/commit/19a0ac680621918042af38e5d7bb16c21c995841))
* emit warning once for empty frontend/backend services ([ebe5ea6](https://github.com/HBTGmbH/k8s-httpcache/commit/ebe5ea6b9d4687151c01f12d5c4f0aa68079c7e5))
* implement native connection draining feature ([c91e645](https://github.com/HBTGmbH/k8s-httpcache/commit/c91e6454295485991fff148e13e8b15621929b48))
* implement retry for VCL reloading ([d551b54](https://github.com/HBTGmbH/k8s-httpcache/commit/d551b5410fd9d94d55e4ba1b961ec566e6cd37bb))
* initial commit ([211126d](https://github.com/HBTGmbH/k8s-httpcache/commit/211126d1d5a6186925fbb00d20895b59ae77c96c))
* log warning whenever any backend has 0 endpoints ([e95bd3b](https://github.com/HBTGmbH/k8s-httpcache/commit/e95bd3b720c65fb8f7faeb62194f7471a94c4084))
* make debounce latency histogram configurable and add debounce-related metrics ([5235f06](https://github.com/HBTGmbH/k8s-httpcache/commit/5235f0698cdf3fd2216fe5794da7ed0152e4f116))
* make file-system watching configurable ([6a33131](https://github.com/HBTGmbH/k8s-httpcache/commit/6a33131ab5347da582c0465175aae93c6415ca6d))
* make more settings configurable ([30aceec](https://github.com/HBTGmbH/k8s-httpcache/commit/30aceec24059d9b471ac498863e1909b276b882e))
* make shutdown timeout configurable ([d4917e9](https://github.com/HBTGmbH/k8s-httpcache/commit/d4917e91819cad3453dd720aae61c0bb2a8de085))
* output only the version with --version ([72b9e09](https://github.com/HBTGmbH/k8s-httpcache/commit/72b9e099566f01fd2141e835e7893fc443ee8571))
* support ConfigMap and filesystem values in VCL templates ([9e7ecad](https://github.com/HBTGmbH/k8s-httpcache/commit/9e7ecad0e57d5c40f37878844b80497c2985a272))
* switch from Go's flag to urfave/cli for config parsing ([bc27189](https://github.com/HBTGmbH/k8s-httpcache/commit/bc27189376509f8ea9b95be9bc1702932edfc7bb))
* validate backend service name ([39f772e](https://github.com/HBTGmbH/k8s-httpcache/commit/39f772e41e70e736d580792a49ce8b83debe160f))


### Bug Fixes

* do not remove 'import std;' in comments ([a2cf8d1](https://github.com/HBTGmbH/k8s-httpcache/commit/a2cf8d14dd792a77cd26c69e237307753a42a5b0))
