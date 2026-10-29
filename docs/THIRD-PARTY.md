# Third-party dependency inventory

Generated from the resolved dependency graphs of this release. Each entry lists the
module or package, the version actually selected by `go list -m all` / `npm ls`, and the
license found in that package's own source. Licenses are reported as the packages
themselves declare them; this is an inventory, not a legal review.

The remote-executor (RPC) framework that earlier development builds vendored under
`third_party/` is **not** part of this release: it contributes no dependency and no
license here, and the build does not reference it.

Four entries report "no LICENSE file at module root" because the license lives in the
parent module directory rather than beside the package: the three `cloudwego/eino-ext`
sub-modules and `volcengine/volc-sdk-golang`. Their parent modules are listed with
their detected license in the same table.

## Go module: v2

Modules that actually contribute packages to this build:

| Module | Version | License |
|---|---|---|
| `GopherAI` | `` | this repository |
| `github.com/BurntSushi/toml` | `v0.3.1` | MIT |
| `github.com/cespare/xxhash/v2` | `v2.3.0` | MIT |
| `github.com/dgryski/go-rendezvous` | `v0.0.0-20200823014737-9f7001d12a5f` | MIT |
| `github.com/redis/go-redis/v9` | `v9.16.0` | BSD-3-Clause |
| `github.com/cloudwego/eino` | `v0.5.14` | Apache-2.0 |
| `github.com/bytedance/gopkg` | `v0.1.3` | Apache-2.0 |
| `github.com/bytedance/sonic` | `v1.15.2` | Apache-2.0 |
| `github.com/bytedance/sonic/loader` | `v0.5.1` | Apache-2.0 |
| `github.com/cloudwego/base64x` | `v0.1.6` | Apache-2.0 |
| `github.com/klauspost/cpuid/v2` | `v2.3.0` | MIT |
| `golang.org/x/arch` | `v0.20.0` | BSD-3-Clause |
| `github.com/twitchyliquid64/golang-asm` | `v0.15.1` | BSD-3-Clause |
| `github.com/bahlo/generic-list-go` | `v0.2.0` | BSD-3-Clause |
| `github.com/buger/jsonparser` | `v1.1.1` | MIT |
| `github.com/mailru/easyjson` | `v0.7.7` | MIT |
| `gopkg.in/yaml.v3` | `v3.0.1` | MIT |
| `github.com/wk8/go-ordered-map/v2` | `v2.1.8` | Apache-2.0 |
| `github.com/eino-contrib/jsonschema` | `v1.0.2` | MIT |
| `github.com/josharian/intern` | `v1.0.0` | MIT |
| `gopkg.in/yaml.v2` | `v2.4.0` | Apache-2.0 |
| `github.com/go-openapi/swag` | `v0.19.5` | Apache-2.0 |
| `github.com/go-openapi/jsonpointer` | `v0.19.5` | Apache-2.0 |
| `github.com/invopop/yaml` | `v0.1.0` | MIT |
| `github.com/mohae/deepcopy` | `v0.0.0-20170929034955-c48cc78d4826` | MIT |
| `github.com/perimeterx/marshmallow` | `v1.1.4` | MIT |
| `github.com/getkin/kin-openapi` | `v0.118.0` | MIT |
| `github.com/pkg/errors` | `v0.9.1` | BSD-3-Clause |
| `github.com/goph/emperror` | `v0.17.2` | MIT |
| `github.com/dustin/go-humanize` | `v1.0.1` | MIT |
| `github.com/modern-go/concurrent` | `v0.0.0-20180306012644-bacd9c7ef1dd` | Apache-2.0 |
| `github.com/modern-go/reflect2` | `v1.0.2` | Apache-2.0 |
| `github.com/json-iterator/go` | `v1.1.12` | MIT |
| `github.com/nikolalohinski/gonja` | `v1.5.3` | MIT |
| `golang.org/x/sys` | `v0.46.0` | BSD-3-Clause |
| `github.com/sirupsen/logrus` | `v1.9.3` | MIT |
| `github.com/pelletier/go-toml/v2` | `v2.2.4` | MIT |
| `github.com/yargevad/filepathx` | `v1.0.0` | MIT |
| `golang.org/x/exp` | `v0.0.0-20250218142911-aa4b98e5adaa` | BSD-3-Clause |
| `github.com/slongfield/pyfmt` | `v0.0.0-20220222012616-ea85ff4c361f` | BSD-3-Clause |
| `github.com/jinzhu/now` | `v1.1.5` | MIT |
| `gorm.io/gorm` | `v1.31.1` | MIT |
| `github.com/jinzhu/inflection` | `v1.0.0` | MIT |
| `golang.org/x/text` | `v0.39.0` | BSD-3-Clause |
| `github.com/gin-contrib/sse` | `v1.1.0` | MIT |
| `github.com/gin-gonic/gin` | `v1.11.0` | MIT |
| `golang.org/x/net` | `v0.56.0` | BSD-3-Clause |
| `github.com/gabriel-vasile/mimetype` | `v1.4.8` | MIT |
| `github.com/go-playground/locales` | `v0.14.1` | MIT |
| `github.com/go-playground/universal-translator` | `v0.18.1` | MIT |
| `github.com/leodido/go-urn` | `v1.4.0` | MIT |
| `golang.org/x/crypto` | `v0.53.0` | BSD-3-Clause |
| `github.com/go-playground/validator/v10` | `v10.27.0` | MIT |
| `github.com/goccy/go-yaml` | `v1.18.0` | MIT |
| `github.com/ugorji/go/codec` | `v1.3.0` | MIT |
| `google.golang.org/protobuf` | `v1.36.11` | BSD-3-Clause |
| `github.com/mattn/go-isatty` | `v0.0.20` | MIT |
| `github.com/quic-go/qpack` | `v0.6.0` | MIT |
| `github.com/quic-go/quic-go` | `v0.59.1` | MIT |
| `filippo.io/edwards25519` | `v1.1.0` | BSD-3-Clause |
| `github.com/go-sql-driver/mysql` | `v1.8.1` | MPL-2.0 |
| `gorm.io/driver/mysql` | `v1.6.0` | MIT |
| `gopkg.in/gomail.v2` | `v2.0.0-20160411212932-81ebce5c23df` | MIT |
| `github.com/google/uuid` | `v1.6.0` | BSD-3-Clause |
| `github.com/evanphx/json-patch` | `v0.5.2` | BSD-3-Clause |
| `github.com/meguminnnnnnnnn/go-openai` | `v0.1.0` | Apache-2.0 |
| `github.com/cloudwego/eino-ext/libs/acl/openai` | `v0.1.1` | no LICENSE file at module root |
| `github.com/cloudwego/eino-ext/components/model/openai` | `v0.1.4` | no LICENSE file at module root |
| `github.com/golang-jwt/jwt/v4` | `v4.5.2` | MIT |
| `github.com/volcengine/volcengine-go-sdk` | `v1.1.44` | Apache-2.0 |
| `github.com/volcengine/volc-sdk-golang` | `v1.0.23` | no LICENSE file at module root |
| `github.com/jmespath/go-jmespath` | `v0.4.0` | Apache-2.0 |
| `golang.org/x/sync` | `v0.21.0` | BSD-3-Clause |
| `github.com/cloudwego/eino-ext/components/embedding/ark` | `v0.1.0` | no LICENSE file at module root |

## Go module: hybrid

Modules that actually contribute packages to this build:

| Module | Version | License |
|---|---|---|
| `GopherAI` | `v0.0.0` | this repository |
| `github.com/BurntSushi/toml` | `v0.3.1` | MIT |
| `github.com/cespare/xxhash/v2` | `v2.3.0` | MIT |
| `github.com/dgryski/go-rendezvous` | `v0.0.0-20200823014737-9f7001d12a5f` | MIT |
| `github.com/redis/go-redis/v9` | `v9.16.0` | BSD-3-Clause |
| `github.com/cloudwego/eino` | `v0.5.14` | Apache-2.0 |
| `github.com/bytedance/gopkg` | `v0.1.3` | Apache-2.0 |
| `github.com/bytedance/sonic` | `v1.15.2` | Apache-2.0 |
| `github.com/bytedance/sonic/loader` | `v0.5.1` | Apache-2.0 |
| `github.com/cloudwego/base64x` | `v0.1.6` | Apache-2.0 |
| `github.com/klauspost/cpuid/v2` | `v2.3.0` | MIT |
| `golang.org/x/arch` | `v0.22.0` | BSD-3-Clause |
| `github.com/twitchyliquid64/golang-asm` | `v0.15.1` | BSD-3-Clause |
| `github.com/bahlo/generic-list-go` | `v0.2.0` | BSD-3-Clause |
| `github.com/buger/jsonparser` | `v1.1.1` | MIT |
| `github.com/mailru/easyjson` | `v0.7.7` | MIT |
| `gopkg.in/yaml.v3` | `v3.0.1` | MIT |
| `github.com/wk8/go-ordered-map/v2` | `v2.1.8` | Apache-2.0 |
| `github.com/eino-contrib/jsonschema` | `v1.0.2` | MIT |
| `github.com/josharian/intern` | `v1.0.0` | MIT |
| `gopkg.in/yaml.v2` | `v2.4.0` | Apache-2.0 |
| `github.com/go-openapi/swag` | `v0.19.5` | Apache-2.0 |
| `github.com/go-openapi/jsonpointer` | `v0.19.5` | Apache-2.0 |
| `github.com/invopop/yaml` | `v0.1.0` | MIT |
| `github.com/mohae/deepcopy` | `v0.0.0-20170929034955-c48cc78d4826` | MIT |
| `github.com/perimeterx/marshmallow` | `v1.1.4` | MIT |
| `github.com/getkin/kin-openapi` | `v0.118.0` | MIT |
| `github.com/pkg/errors` | `v0.9.1` | BSD-3-Clause |
| `github.com/goph/emperror` | `v0.17.2` | MIT |
| `github.com/dustin/go-humanize` | `v1.0.1` | MIT |
| `github.com/modern-go/concurrent` | `v0.0.0-20180306012644-bacd9c7ef1dd` | Apache-2.0 |
| `github.com/modern-go/reflect2` | `v1.0.2` | Apache-2.0 |
| `github.com/json-iterator/go` | `v1.1.12` | MIT |
| `github.com/nikolalohinski/gonja` | `v1.5.3` | MIT |
| `golang.org/x/sys` | `v0.46.0` | BSD-3-Clause |
| `github.com/sirupsen/logrus` | `v1.9.3` | MIT |
| `github.com/pelletier/go-toml/v2` | `v2.2.4` | MIT |
| `github.com/yargevad/filepathx` | `v1.0.0` | MIT |
| `golang.org/x/exp` | `v0.0.0-20250218142911-aa4b98e5adaa` | BSD-3-Clause |
| `github.com/slongfield/pyfmt` | `v0.0.0-20220222012616-ea85ff4c361f` | BSD-3-Clause |
| `github.com/jinzhu/now` | `v1.1.5` | MIT |
| `gorm.io/gorm` | `v1.31.1` | MIT |
| `github.com/jinzhu/inflection` | `v1.0.0` | MIT |
| `golang.org/x/text` | `v0.39.0` | BSD-3-Clause |
| `github.com/gin-contrib/sse` | `v1.1.0` | MIT |
| `github.com/gin-gonic/gin` | `v1.11.0` | MIT |
| `github.com/gabriel-vasile/mimetype` | `v1.4.12` | MIT |
| `github.com/go-playground/locales` | `v0.14.1` | MIT |
| `github.com/go-playground/universal-translator` | `v0.18.1` | MIT |
| `github.com/leodido/go-urn` | `v1.4.0` | MIT |
| `golang.org/x/crypto` | `v0.53.0` | BSD-3-Clause |
| `github.com/go-playground/validator/v10` | `v10.30.1` | MIT |
| `github.com/goccy/go-yaml` | `v1.18.0` | MIT |
| `github.com/ugorji/go/codec` | `v1.3.1` | MIT |
| `google.golang.org/protobuf` | `v1.36.11` | BSD-3-Clause |
| `github.com/mattn/go-isatty` | `v0.0.20` | MIT |
| `golang.org/x/net` | `v0.56.0` | BSD-3-Clause |
| `github.com/quic-go/qpack` | `v0.6.0` | MIT |
| `github.com/quic-go/quic-go` | `v0.59.1` | MIT |
| `filippo.io/edwards25519` | `v1.1.0` | BSD-3-Clause |
| `github.com/go-sql-driver/mysql` | `v1.8.1` | MPL-2.0 |
| `gorm.io/driver/mysql` | `v1.6.0` | MIT |
| `github.com/evanphx/json-patch` | `v0.5.2` | BSD-3-Clause |
| `github.com/meguminnnnnnnnn/go-openai` | `v0.1.0` | Apache-2.0 |
| `github.com/cloudwego/eino-ext/libs/acl/openai` | `v0.1.1` | no LICENSE file at module root |
| `github.com/cloudwego/eino-ext/components/model/openai` | `v0.1.4` | no LICENSE file at module root |
| `github.com/google/uuid` | `v1.6.0` | BSD-3-Clause |
| `gopkg.in/gomail.v2` | `v2.0.0-20160411212932-81ebce5c23df` | MIT |
| `github.com/golang-jwt/jwt/v4` | `v4.5.2` | MIT |
| `github.com/volcengine/volcengine-go-sdk` | `v1.1.44` | Apache-2.0 |
| `github.com/volcengine/volc-sdk-golang` | `v1.0.23` | no LICENSE file at module root |
| `github.com/jmespath/go-jmespath` | `v0.4.0` | Apache-2.0 |
| `golang.org/x/sync` | `v0.21.0` | BSD-3-Clause |
| `github.com/cloudwego/eino-ext/components/embedding/ark` | `v0.1.0` | no LICENSE file at module root |
| `github.com/xiaopang456/GopherLearn` | `v0.0.0` | this repository |
| `gopherlearn-v2-hybrid` | `` | no LICENSE file at module root |

## Go module: root

Modules that actually contribute packages to this build:

| Module | Version | License |
|---|---|---|
| `github.com/xiaopang456/GopherLearn` | `` | this repository |
| `github.com/cespare/xxhash/v2` | `v2.3.0` | MIT |
| `github.com/dgryski/go-rendezvous` | `v0.0.0-20200823014737-9f7001d12a5f` | MIT |
| `github.com/redis/go-redis/v9` | `v9.16.0` | BSD-3-Clause |
| `golang.org/x/crypto` | `v0.48.0` | BSD-3-Clause |
| `filippo.io/edwards25519` | `v1.1.0` | BSD-3-Clause |
| `github.com/go-sql-driver/mysql` | `v1.8.1` | MPL-2.0 |
| `github.com/jinzhu/now` | `v1.1.5` | MIT |
| `gorm.io/gorm` | `v1.31.1` | MIT |
| `github.com/jinzhu/inflection` | `v1.0.0` | MIT |
| `golang.org/x/text` | `v0.34.0` | BSD-3-Clause |
| `gorm.io/driver/mysql` | `v1.6.0` | MIT |
| `github.com/gin-contrib/sse` | `v1.1.0` | MIT |
| `github.com/gin-gonic/gin` | `v1.10.1` | MIT |
| `github.com/gabriel-vasile/mimetype` | `v1.4.12` | MIT |
| `github.com/go-playground/locales` | `v0.14.1` | MIT |
| `github.com/go-playground/universal-translator` | `v0.18.1` | MIT |
| `github.com/leodido/go-urn` | `v1.4.0` | MIT |
| `golang.org/x/sys` | `v0.41.0` | BSD-3-Clause |
| `github.com/go-playground/validator/v10` | `v10.30.1` | MIT |
| `github.com/pelletier/go-toml/v2` | `v2.2.4` | MIT |
| `github.com/ugorji/go/codec` | `v1.3.1` | MIT |
| `google.golang.org/protobuf` | `v1.36.10` | BSD-3-Clause |
| `gopkg.in/yaml.v3` | `v3.0.1` | MIT |
| `github.com/mattn/go-isatty` | `v0.0.20` | MIT |
| `golang.org/x/net` | `v0.49.0` | BSD-3-Clause |

## npm packages (`web`)

| Package | Version | License |
|---|---|---|
| `@asamuzakjp/css-color` | `3.2.0` | MIT |
| `@babel/helper-string-parser` | `7.29.7` | MIT |
| `@babel/helper-validator-identifier` | `7.29.7` | MIT |
| `@babel/parser` | `7.29.9` | MIT |
| `@babel/types` | `7.29.8` | MIT |
| `@csstools/color-helpers` | `5.1.0` | MIT-0 |
| `@csstools/css-calc` | `2.1.4` | MIT |
| `@csstools/css-color-parser` | `3.1.0` | MIT |
| `@csstools/css-parser-algorithms` | `3.0.5` | MIT |
| `@csstools/css-tokenizer` | `3.0.4` | MIT |
| `@esbuild/win32-x64` | `0.25.12` | MIT |
| `@eslint/config-array` | `0.21.2` | Apache-2.0 |
| `@eslint/config-helpers` | `0.4.2` | Apache-2.0 |
| `@eslint/core` | `0.17.0` | Apache-2.0 |
| `@eslint/eslintrc` | `3.3.7` | MIT |
| `@eslint/js` | `9.39.5` | MIT |
| `@eslint/object-schema` | `2.1.7` | Apache-2.0 |
| `@eslint/plugin-kit` | `0.4.1` | Apache-2.0 |
| `@eslint-community/eslint-utils` | `4.10.1` | MIT |
| `@eslint-community/regexpp` | `4.12.2` | MIT |
| `@humanfs/core` | `0.19.2` | Apache-2.0 |
| `@humanfs/node` | `0.16.8` | Apache-2.0 |
| `@humanfs/types` | `0.15.0` | Apache-2.0 |
| `@humanwhocodes/module-importer` | `1.0.1` | Apache-2.0 |
| `@humanwhocodes/retry` | `0.4.3` | Apache-2.0 |
| `@jridgewell/resolve-uri` | `3.1.2` | MIT |
| `@jridgewell/sourcemap-codec` | `1.6.0` | MIT |
| `@jridgewell/trace-mapping` | `0.3.31` | MIT |
| `@one-ini/wasm` | `0.2.1` | MIT |
| `@rollup/rollup-win32-x64-gnu` | `4.63.5` | MIT |
| `@rollup/rollup-win32-x64-msvc` | `4.63.5` | MIT |
| `@types/chai` | `5.2.3` | MIT |
| `@types/deep-eql` | `4.0.2` | MIT |
| `@types/estree` | `1.0.9` | MIT |
| `@types/json-schema` | `7.0.15` | MIT |
| `@vitejs/plugin-vue` | `5.2.4` | MIT |
| `@vitest/mocker` | `5.0.1` | MIT |
| `@vitest/spy` | `5.0.1` | MIT |
| `@vue/compiler-core` | `3.5.43` | MIT |
| `@vue/compiler-dom` | `3.5.43` | MIT |
| `@vue/compiler-sfc` | `3.5.43` | MIT |
| `@vue/compiler-ssr` | `3.5.43` | MIT |
| `@vue/devtools-api` | `6.6.4` | MIT |
| `@vue/reactivity` | `3.5.43` | MIT |
| `@vue/runtime-core` | `3.5.43` | MIT |
| `@vue/runtime-dom` | `3.5.43` | MIT |
| `@vue/server-renderer` | `3.5.43` | MIT |
| `@vue/shared` | `3.5.43` | MIT |
| `@vue/test-utils` | `2.5.1` | MIT |
| `abbrev` | `5.0.0` | ISC |
| `acorn` | `8.18.0` | MIT |
| `acorn-jsx` | `5.3.2` | MIT |
| `agent-base` | `7.1.4` | MIT |
| `ajv` | `6.15.0` | MIT |
| `ansi-styles` | `4.3.0` | MIT |
| `argparse` | `2.0.1` | Python-2.0 |
| `assertion-error` | `2.0.1` | MIT |
| `balanced-match` | `1.0.2` | MIT |
| `boolbase` | `1.0.0` | ISC |
| `brace-expansion` | `1.1.21` | MIT |
| `callsites` | `3.1.0` | MIT |
| `chai` | `6.2.2` | MIT |
| `chalk` | `4.1.2` | MIT |
| `color-convert` | `2.0.1` | MIT |
| `color-name` | `1.1.4` | MIT |
| `commander` | `14.0.3` | MIT |
| `concat-map` | `0.0.1` | MIT |
| `config-chain` | `1.1.13` | @{type=MIT; url=https://raw.githubusercontent.com/dominictarr/config-chain/master/LICENCE} |
| `cross-spawn` | `7.0.6` | MIT |
| `cssesc` | `3.0.0` | MIT |
| `cssstyle` | `4.6.0` | MIT |
| `csstype` | `3.2.3` | MIT |
| `data-urls` | `5.0.0` | MIT |
| `debug` | `4.4.3` | MIT |
| `decimal.js` | `10.6.0` | MIT |
| `deep-is` | `0.1.4` | MIT |
| `editorconfig` | `3.0.2` | MIT |
| `entities` | `7.0.1` | BSD-2-Clause |
| `esbuild` | `0.25.12` | MIT |
| `escape-string-regexp` | `4.0.0` | MIT |
| `eslint` | `9.39.5` | MIT |
| `eslint-plugin-vue` | `9.33.0` | MIT |
| `eslint-scope` | `8.4.0` | BSD-2-Clause |
| `eslint-visitor-keys` | `4.2.1` | Apache-2.0 |
| `es-module-lexer` | `2.3.2` | MIT |
| `espree` | `10.4.0` | BSD-2-Clause |
| `esquery` | `1.7.0` | BSD-3-Clause |
| `esrecurse` | `4.3.0` | BSD-2-Clause |
| `estraverse` | `5.3.0` | BSD-2-Clause |
| `estree-walker` | `2.0.2` | MIT |
| `esutils` | `2.0.3` | BSD-2-Clause |
| `expect-type` | `1.4.0` | Apache-2.0 |
| `fast-deep-equal` | `3.1.3` | MIT |
| `fast-json-stable-stringify` | `2.1.0` | MIT |
| `fast-levenshtein` | `2.0.6` | MIT |
| `fdir` | `6.5.0` | MIT |
| `file-entry-cache` | `8.0.0` | MIT |
| `find-up` | `5.0.0` | MIT |
| `flat-cache` | `4.0.1` | MIT |
| `flatted` | `3.4.4` | ISC |
| `glob` | `13.0.6` | BlueOak-1.0.0 |
| `globals` | `15.15.0` | MIT |
| `glob-parent` | `6.0.2` | ISC |
| `has-flag` | `4.0.0` | MIT |
| `html-encoding-sniffer` | `4.0.0` | MIT |
| `http-proxy-agent` | `7.0.2` | MIT |
| `https-proxy-agent` | `7.0.6` | MIT |
| `iconv-lite` | `0.6.3` | MIT |
| `ignore` | `5.3.2` | MIT |
| `import-fresh` | `3.3.1` | MIT |
| `imurmurhash` | `0.1.4` | MIT |
| `ini` | `1.3.8` | ISC |
| `isexe` | `2.0.0` | ISC |
| `is-extglob` | `2.1.1` | MIT |
| `is-glob` | `4.0.3` | MIT |
| `is-potential-custom-element-name` | `1.0.1` | MIT |
| `js-beautify` | `2.0.3` | MIT |
| `js-cookie` | `3.0.8` | MIT |
| `jsdom` | `26.1.0` | MIT |
| `json-buffer` | `3.0.1` | MIT |
| `json-schema-traverse` | `0.4.1` | MIT |
| `json-stable-stringify-without-jsonify` | `1.0.1` | MIT |
| `js-yaml` | `4.3.2` | MIT |
| `keyv` | `4.5.4` | MIT |
| `levn` | `0.4.1` | MIT |
| `locate-path` | `6.0.0` | MIT |
| `lodash` | `4.18.1` | MIT |
| `lodash.merge` | `4.6.2` | MIT |
| `lru-cache` | `10.4.3` | ISC |
| `magic-string` | `0.30.21` | MIT |
| `marked` | `15.0.12` | MIT |
| `minimatch` | `3.1.5` | ISC |
| `minipass` | `7.1.3` | BlueOak-1.0.0 |
| `ms` | `2.1.3` | MIT |
| `nanoid` | `3.3.19` | MIT |
| `natural-compare` | `1.4.0` | MIT |
| `nopt` | `10.0.1` | ISC |
| `nth-check` | `2.1.1` | BSD-2-Clause |
| `nwsapi` | `2.2.28` | MIT |
| `obug` | `2.2.1` | MIT |
| `optionator` | `0.9.4` | MIT |
| `parent-module` | `1.0.1` | MIT |
| `parse5` | `7.3.0` | MIT |
| `path-exists` | `4.0.0` | MIT |
| `path-key` | `3.1.1` | MIT |
| `path-scurry` | `2.0.2` | BlueOak-1.0.0 |
| `picocolors` | `1.1.1` | ISC |
| `picomatch` | `4.0.7` | MIT |
| `p-limit` | `3.1.0` | MIT |
| `p-locate` | `5.0.0` | MIT |
| `postcss` | `8.5.28` | MIT |
| `postcss-selector-parser` | `6.1.4` | MIT |
| `prelude-ls` | `1.2.1` | MIT |
| `proto-list` | `1.2.4` | ISC |
| `punycode` | `2.3.1` | MIT |
| `resolve-from` | `4.0.0` | MIT |
| `rollup` | `4.63.5` | MIT |
| `rrweb-cssom` | `0.8.0` | MIT |
| `safer-buffer` | `2.1.2` | MIT |
| `saxes` | `6.0.0` | ISC |
| `semver` | `7.8.5` | ISC |
| `shebang-command` | `2.0.0` | MIT |
| `shebang-regex` | `3.0.0` | MIT |
| `siginfo` | `2.0.0` | ISC |
| `source-map-js` | `1.2.1` | BSD-3-Clause |
| `stackback` | `0.0.2` | MIT |
| `std-env` | `4.2.0` | MIT |
| `strip-json-comments` | `3.1.1` | MIT |
| `supports-color` | `7.2.0` | MIT |
| `symbol-tree` | `3.2.4` | MIT |
| `tinybench` | `6.1.4` | MIT |
| `tinyexec` | `1.3.0` | MIT |
| `tinyglobby` | `0.2.17` | MIT |
| `tldts` | `6.1.86` | MIT |
| `tldts-core` | `6.1.86` | MIT |
| `tough-cookie` | `5.1.2` | BSD-3-Clause |
| `tr46` | `5.1.1` | MIT |
| `type-check` | `0.4.0` | MIT |
| `type-fest` | `0.20.2` | (MIT OR CC0-1.0) |
| `uri-js` | `4.4.1` | BSD-2-Clause |
| `util-deprecate` | `1.0.2` | MIT |
| `vite` | `6.4.3` | MIT |
| `vitest` | `5.0.1` | MIT |
| `vue` | `3.5.43` | MIT |
| `vue-component-type-helpers` | `3.3.11` | MIT |
| `vue-eslint-parser` | `9.4.3` | MIT |
| `vue-router` | `4.6.4` | MIT |
| `w3c-xmlserializer` | `5.0.0` | MIT |
| `webidl-conversions` | `7.0.0` | BSD-2-Clause |
| `whatwg-encoding` | `3.1.1` | MIT |
| `whatwg-mimetype` | `4.0.0` | MIT |
| `whatwg-url` | `14.2.0` | MIT |
| `which` | `2.0.2` | ISC |
| `why-is-node-running` | `2.3.0` | MIT |
| `word-wrap` | `1.2.5` | MIT |
| `ws` | `8.21.3` | MIT |
| `xmlchars` | `2.2.0` | MIT |
| `xml-name-validator` | `4.0.0` | Apache-2.0 |
| `yocto-queue` | `0.1.0` | MIT |
