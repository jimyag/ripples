module github.com/jimyag/ripples

go 1.25

require (
	github.com/sourcegraph/go-diff v0.7.0
	golang.org/x/tools v0.38.0
	golang.org/x/tools/gopls v0.0.0
)

require (
	github.com/BurntSushi/toml v1.5.0 // indirect
	github.com/fatih/camelcase v1.0.0 // indirect
	github.com/fatih/gomodifytags v1.17.1-0.20250423142747-f3939df9aa3c // indirect
	github.com/fatih/structtag v1.2.0 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/mattn/go-colorable v0.1.13 // indirect
	github.com/mattn/go-isatty v0.0.19 // indirect
	github.com/rs/zerolog v1.34.0 // indirect
	golang.org/x/exp/typeparams v0.0.0-20251023183803-a4bb9ffd2546 // indirect
	golang.org/x/mod v0.30.0 // indirect
	golang.org/x/sync v0.18.0 // indirect
	golang.org/x/sys v0.38.0 // indirect
	golang.org/x/telemetry v0.0.0-20251111182119-bc8e575c7b54 // indirect
	golang.org/x/text v0.31.0 // indirect
	golang.org/x/vuln v1.1.4 // indirect
	honnef.co/go/tools v0.7.0-0.dev.0.20251022135355-8273271481d0 // indirect
	mvdan.cc/gofumpt v0.8.0 // indirect
)

// 使用本地 tools 仓库进行开发
replace golang.org/x/tools => ../golang-tools

replace golang.org/x/tools/gopls => ../golang-tools/gopls
