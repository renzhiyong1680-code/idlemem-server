module github.com/xiancuncloud/idlemem-server

go 1.27

require (
	github.com/tidwall/buntdb v1.3.2
	github.com/tidwall/redcon v1.6.2
	github.com/xiancuncloud/idlemem-core v0.0.0-00010101000000-000000000000
)

require (
	github.com/tidwall/btree v1.4.2 // indirect
	github.com/tidwall/gjson v1.14.3 // indirect
	github.com/tidwall/grect v0.1.4 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.0 // indirect
	github.com/tidwall/rtred v0.1.2 // indirect
	github.com/tidwall/tinyqueue v0.1.1 // indirect
)

replace github.com/xiancuncloud/idlemem-core => ../idlemem-core
