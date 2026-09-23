module github.com/xiatianliang1024gm/sqldb

go 1.23

require github.com/xiatianliang1024gm/kvdb v0.0.0

require github.com/golang/snappy v1.0.0 // indirect

// 本地开发用 replace 直接指向旁边的 kvdb 仓库，不依赖网络。
// 推到远端给别人用时，把这一行删掉（届时 go get 需要能解析 kvdb 的 tag）。
replace github.com/xiatianliang1024gm/kvdb => ../kvdb
