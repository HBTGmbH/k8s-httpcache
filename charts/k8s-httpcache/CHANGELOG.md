# Changelog

## [1.1.0](https://github.com/HBTGmbH/k8s-httpcache/compare/k8s-httpcache-v1.0.0...k8s-httpcache-v1.1.0) (2026-06-20)


### Features

* **chart:** add additional optional resources to Helm chart ([01ad969](https://github.com/HBTGmbH/k8s-httpcache/commit/01ad969b9388c90e5f8277d011980af292670c37))
* **chart:** add more optional resources to Helm chart ([0b963fb](https://github.com/HBTGmbH/k8s-httpcache/commit/0b963fbf7c3a6b53d7eea6e9c51c1ef405a98f20))
* **chart:** add support for serving static files ([2db9f76](https://github.com/HBTGmbH/k8s-httpcache/commit/2db9f7650bc29d9ba6a81c927640c315edbe64ba))
* **chart:** enable frontend-sharding and drain by default ([bb48a9a](https://github.com/HBTGmbH/k8s-httpcache/commit/bb48a9a4b16a09034db0050e62cdbd751a8af7f7))
* **chart:** only compute VCL checksum annotation if not vcl.fileWatch ([b422cd9](https://github.com/HBTGmbH/k8s-httpcache/commit/b422cd9eb4403fa0d936f498e1205ec6faeb7b85))
* **chart:** support Helm templating within the VCL template ([f62310a](https://github.com/HBTGmbH/k8s-httpcache/commit/f62310afd4392c30416cf053ea27e2ae9ac12c1e))
* separate VCL watch from values dir watch ([915d323](https://github.com/HBTGmbH/k8s-httpcache/commit/915d323d22efd6bfb02e493bf0bc214e49a49bfb))


### Bug Fixes

* **chart:** use Helm value for HTTP metrics port ([17c440c](https://github.com/HBTGmbH/k8s-httpcache/commit/17c440cccbd5d49fb0fc884d9ad0e6909237adf0))

## 1.0.0 (2026-06-15)


### Features

* add dynamic backend discovery ([bd9f3f9](https://github.com/HBTGmbH/k8s-httpcache/commit/bd9f3f91af99d01f41cd047b0cbec8acea6cd09f))
* add Helm chart [skip ci] ([7571b3c](https://github.com/HBTGmbH/k8s-httpcache/commit/7571b3c992e8de1dc2aa3fd3c678887a8aa4fe36))
* add support for Vinyl Cache ([97e1d9a](https://github.com/HBTGmbH/k8s-httpcache/commit/97e1d9addaa72f3b2592cf39476d7a6ce0604f63))
* add TLS support for Varnish 9+ ([438be56](https://github.com/HBTGmbH/k8s-httpcache/commit/438be56585988631abad3e43d6c98fa9b24cdad4))
* make container ports configurable in Helm chart ([3b5795f](https://github.com/HBTGmbH/k8s-httpcache/commit/3b5795faad4c59c93a28159c0c4241a912cad1e0))
* rename .IP to .Host in frontend/backend VCL template model ([732f841](https://github.com/HBTGmbH/k8s-httpcache/commit/732f841ee8018ef62ebf0d171a5cefcec70230aa))
