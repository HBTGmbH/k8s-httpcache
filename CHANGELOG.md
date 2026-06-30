# Changelog

## [1.2.3](https://github.com/HBTGmbH/k8s-httpcache/compare/v1.2.2...v1.2.3) (2026-06-30)


### Bug Fixes

* go.mod module path ([9716014](https://github.com/HBTGmbH/k8s-httpcache/commit/9716014446d5dd4eacabf159f516379b8a3876b2))
* race in collectInitialState ([4f082f7](https://github.com/HBTGmbH/k8s-httpcache/commit/4f082f7928ebb10139052266cd9c6166df453244))

## [1.2.2](https://github.com/HBTGmbH/k8s-httpcache/compare/v1.2.1...v1.2.2) (2026-06-27)


### Bug Fixes

* config validation and VCL comment parsing ([89400a9](https://github.com/HBTGmbH/k8s-httpcache/commit/89400a960fa744771395f735e9e760948b0accb0))
* make watcher/discovery more robust ([df786d7](https://github.com/HBTGmbH/k8s-httpcache/commit/df786d7021ea4af7edd61be01e6015a365831ebb))

## [1.2.1](https://github.com/HBTGmbH/k8s-httpcache/compare/v1.2.0...v1.2.1) (2026-06-26)


### Bug Fixes

* some minor concurrency issues ([09af0e5](https://github.com/HBTGmbH/k8s-httpcache/commit/09af0e585bae938149f93377b670aec2edc42e29))

## [1.2.0](https://github.com/HBTGmbH/k8s-httpcache/compare/v1.1.0...v1.2.0) (2026-06-19)


### Features

* **chart:** add support for serving static files ([2db9f76](https://github.com/HBTGmbH/k8s-httpcache/commit/2db9f7650bc29d9ba6a81c927640c315edbe64ba))
* **metrics:** add per-pod broadcast and TLS cert-expiry metrics ([3469aed](https://github.com/HBTGmbH/k8s-httpcache/commit/3469aed4e643d97cb4d9b54592cae1b1461c5bcc))
* separate VCL watch from values dir watch ([915d323](https://github.com/HBTGmbH/k8s-httpcache/commit/915d323d22efd6bfb02e493bf0bc214e49a49bfb))


### Bug Fixes

* bound broadcast request metric's method label to prevent cardinality leak ([67c9dd9](https://github.com/HBTGmbH/k8s-httpcache/commit/67c9dd9b8c98c1223853f739bad491d829b1325b))
* cap prefixWriter buffer to prevent unbounded growth on newline-free output ([2d4a309](https://github.com/HBTGmbH/k8s-httpcache/commit/2d4a3090a7a4fad8bc5ff76604eb1f0ccf743958))
* correctness bugs in varnishd lifecycle and backend discovery ([2a105d0](https://github.com/HBTGmbH/k8s-httpcache/commit/2a105d0c614bc9dfd2eb3c41b40b598223fa9c0e))
* delete endpoint_updates_total series on discovered backend removal ([7d8260f](https://github.com/HBTGmbH/k8s-httpcache/commit/7d8260f329cb7bdd76d9c9de4c6e454334edb4ad))
* drain vcl_deliver placement and render-error template recovery ([4a4a187](https://github.com/HBTGmbH/k8s-httpcache/commit/4a4a187dad436443a531abe73161cf477bf865af))
* enforce documented --broadcast-write-timeout &gt; read + client constraint ([4a1c5a0](https://github.com/HBTGmbH/k8s-httpcache/commit/4a1c5a0afd369f8b75a96b5dd7194d15212f2e61))
* shut down gracefully instead of os.Exit on HTTP server failure ([267a370](https://github.com/HBTGmbH/k8s-httpcache/commit/267a3706c586d1ed2c07964298160133de28367b))

## [1.1.0](https://github.com/HBTGmbH/k8s-httpcache/compare/v1.0.0...v1.1.0) (2026-06-17)


### Features

* add --startup-timeout flag ([a579a71](https://github.com/HBTGmbH/k8s-httpcache/commit/a579a7100ee9469d199d9d655573a4a0990b539c))
* add DEBUG logging for all event-loop decisions/actions ([63f33eb](https://github.com/HBTGmbH/k8s-httpcache/commit/63f33ebbb9aa14f318a4155746a1c1494b4e8d86))
* add HTTP-related server/client timeouts ([f64f9be](https://github.com/HBTGmbH/k8s-httpcache/commit/f64f9bee585754a2f69d953edd9fdc6ca5ac39ec))
* report TLS info in /status metrics endpoint ([1d702d3](https://github.com/HBTGmbH/k8s-httpcache/commit/1d702d3e3faea9c40f06fb0b87ec3899d078b301))


### Bug Fixes

* backend removal event can cause hang during startup ([76400f4](https://github.com/HBTGmbH/k8s-httpcache/commit/76400f44c08f20730bef906687c04309e0f967d9))
* **metrics:** buffer/slice aliasing of label keys ([194bf52](https://github.com/HBTGmbH/k8s-httpcache/commit/194bf520eb20e34145c75476742c866c0de59a1d))
* **metrics:** don't panic the scrape on label-cardinality mismatch ([1eb0aac](https://github.com/HBTGmbH/k8s-httpcache/commit/1eb0aac3cfe2dddfcecd88ba57395cd4446c8062))
* prevent two --backend-selector watchers from managing the same backend name ([e82f197](https://github.com/HBTGmbH/k8s-httpcache/commit/e82f197103b82be2a0bed8d74d6a98a62298a6d4))
* rediscover same Service across different backend selectors ([c1056c6](https://github.com/HBTGmbH/k8s-httpcache/commit/c1056c619512d5cf0b342b449eb94a61ec89409d))
* stale discovery removal could delete a re-added backend ([7bcb0e2](https://github.com/HBTGmbH/k8s-httpcache/commit/7bcb0e2d8c7c996adaa9c5bce06d1d6dcf864727))
* stale forward update can resurrect a removed discovered backend ([dce2e06](https://github.com/HBTGmbH/k8s-httpcache/commit/dce2e06813ce2813aae4b2f9d9593a2563b7c043))

## 1.0.0 (2026-06-15)


### Features

* add --debounce-max duration ([aa82e9a](https://github.com/HBTGmbH/k8s-httpcache/commit/aa82e9ad67eef391d7f6b16bc5475adb05db6a01))
* add /healthz and /readyz endpoints to metrics server ([6131418](https://github.com/HBTGmbH/k8s-httpcache/commit/6131418dab7d30d2f2505e6be159c7f93f45aa2c))
* add /status endpoint on metrics listener/port ([2761bb9](https://github.com/HBTGmbH/k8s-httpcache/commit/2761bb9689dcd4f050ccb04c2bb34e08ce792773))
* add broadcast support and add more tests ([a3e74c4](https://github.com/HBTGmbH/k8s-httpcache/commit/a3e74c4b5600cd9aed90e92d0a452a8ebc2d25a8))
* add built-in varnishncsa logging ([5211b0e](https://github.com/HBTGmbH/k8s-httpcache/commit/5211b0e982dd85fa045ec13354856800433fc64b))
* add config to specify kept old VCL objects after reload ([8120e55](https://github.com/HBTGmbH/k8s-httpcache/commit/8120e559d91bda9b6b43bca155e9acbc7f59952e))
* add configurable VCL templating delimiters ([9e4033e](https://github.com/HBTGmbH/k8s-httpcache/commit/9e4033e555028ce6d2ab4ee8a5af64ffc40ce0e0))
* add dynamic backend discovery ([bd9f3f9](https://github.com/HBTGmbH/k8s-httpcache/commit/bd9f3f91af99d01f41cd047b0cbec8acea6cd09f))
* add emitting Kubernetes events and add more metrics ([c49f628](https://github.com/HBTGmbH/k8s-httpcache/commit/c49f62850da70b07fb898ba4e6577682f67edaf0))
* add Helm chart [skip ci] ([7571b3c](https://github.com/HBTGmbH/k8s-httpcache/commit/7571b3c992e8de1dc2aa3fd3c678887a8aa4fe36))
* add log format config ([894a8b8](https://github.com/HBTGmbH/k8s-httpcache/commit/894a8b85a7451c5e2518250d98b86ab29fad3251))
* add Masterminds/sprig templating functions ([15e4965](https://github.com/HBTGmbH/k8s-httpcache/commit/15e49657244b55195b80de5258effe0d1725d5c9))
* add Prometheus metrics ([a49ffaf](https://github.com/HBTGmbH/k8s-httpcache/commit/a49ffafafa2fdcbd0ea7056bb1715280eba7a214))
* add separate backend/frontend debounce configs ([337298e](https://github.com/HBTGmbH/k8s-httpcache/commit/337298e50d0e6607a40f0bf39ed27fd9b398fa53))
* add support for --version ([b1e4fac](https://github.com/HBTGmbH/k8s-httpcache/commit/b1e4fac3f30e6a2a421cbe206ebe1c3029f19a89))
* add support for 'trunk' Varnish version ([16f0fa9](https://github.com/HBTGmbH/k8s-httpcache/commit/16f0fa99cc52413d650097f75bfa1229d34b2c55))
* add support for ExternalName backends ([64f0298](https://github.com/HBTGmbH/k8s-httpcache/commit/64f029844e4be373dc682c9e8205470c40ddfbd3))
* add support for Service annotations in templates ([ce470f3](https://github.com/HBTGmbH/k8s-httpcache/commit/ce470f3627c93b276ba769721fbfdf31895a59d3))
* add support for specifying local zone via --zone ([49383b9](https://github.com/HBTGmbH/k8s-httpcache/commit/49383b9d7c441300cfb2e5b06146228e758e349d))
* add support for Sprout template functions ([e740f45](https://github.com/HBTGmbH/k8s-httpcache/commit/e740f45cd3e29b4780e84cbcf5b9b82a9811fef6))
* add support for using secrets in VCL ([0d75cf3](https://github.com/HBTGmbH/k8s-httpcache/commit/0d75cf31b805cc366be8ead55b6cd3219e5572f1))
* add support for Vinyl Cache ([97e1d9a](https://github.com/HBTGmbH/k8s-httpcache/commit/97e1d9addaa72f3b2592cf39476d7a6ce0604f63))
* add TLS support for Varnish 9+ ([438be56](https://github.com/HBTGmbH/k8s-httpcache/commit/438be56585988631abad3e43d6c98fa9b24cdad4))
* add topology-aware routing ([37ebe86](https://github.com/HBTGmbH/k8s-httpcache/commit/37ebe860bc6b044e5f419da55d741296cafe7c8f))
* add Varnish metrics to /metrics ([cd4d036](https://github.com/HBTGmbH/k8s-httpcache/commit/cd4d0362f471795bcb2f989c1ca6df16665ae79e))
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
* only discard old VCL objects that we also created ([cd8a5ec](https://github.com/HBTGmbH/k8s-httpcache/commit/cd8a5ec32d662ec20ce675c38aeb25b84af72522))
* output only the version with --version ([72b9e09](https://github.com/HBTGmbH/k8s-httpcache/commit/72b9e099566f01fd2141e835e7893fc443ee8571))
* rename .IP to .Host in frontend/backend VCL template model ([732f841](https://github.com/HBTGmbH/k8s-httpcache/commit/732f841ee8018ef62ebf0d171a5cefcec70230aa))
* support ConfigMap and filesystem values in VCL templates ([9e7ecad](https://github.com/HBTGmbH/k8s-httpcache/commit/9e7ecad0e57d5c40f37878844b80497c2985a272))
* switch from Go's flag to urfave/cli for config parsing ([bc27189](https://github.com/HBTGmbH/k8s-httpcache/commit/bc27189376509f8ea9b95be9bc1702932edfc7bb))
* use custom json parser for varnishstat output ([5e5e557](https://github.com/HBTGmbH/k8s-httpcache/commit/5e5e5576b2f718015d0b951ac3dfa03299a955f2))
* validate backend service name ([39f772e](https://github.com/HBTGmbH/k8s-httpcache/commit/39f772e41e70e736d580792a49ce8b83debe160f))
* validate more configuration options ([4d9ee25](https://github.com/HBTGmbH/k8s-httpcache/commit/4d9ee25c5506322274ab69e09dd22e8f476d5a6e))


### Bug Fixes

* broadcast host header loss and NCSA crash counter ([e083224](https://github.com/HBTGmbH/k8s-httpcache/commit/e0832243179ad4a9ee22ec056d294bd1657f7699))
* CLI args parsing after urfave/cli upgrade ([51f555b](https://github.com/HBTGmbH/k8s-httpcache/commit/51f555b77a6988f06a599e002f302f52ac40c779))
* do not remove 'import std;' in comments ([a2cf8d1](https://github.com/HBTGmbH/k8s-httpcache/commit/a2cf8d14dd792a77cd26c69e237307753a42a5b0))
* flaky watcher tests ([cffa7ea](https://github.com/HBTGmbH/k8s-httpcache/commit/cffa7ea9257e489f1664dcccb9514154c72be556))
* **lint:** fix linting issues introduced in the last main_test.go change ([8b2509d](https://github.com/HBTGmbH/k8s-httpcache/commit/8b2509d59d17f2761229b46bede292714cdb1aff))
* multiple correctness and race condition bugs ([83f83af](https://github.com/HBTGmbH/k8s-httpcache/commit/83f83af885b57ec93a3a552f268aeb0c842bb36b))
* retry indefinitely with exponential backoff on reload error ([b217401](https://github.com/HBTGmbH/k8s-httpcache/commit/b2174018c0d9da73c3f1d4f665b040eaafbe6cf8))


### Performance Improvements

* improve varnishncsa log processing ([9788577](https://github.com/HBTGmbH/k8s-httpcache/commit/97885779b350f44efaec18e07e806b76de3751c2))
