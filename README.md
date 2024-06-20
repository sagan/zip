This is a fork of the Go [archive/zip][] package to add support for reading split zip files (.z01 + ... + .zXX + .zip).

Notes

- Only reading of split zip files is implemented. Writing is NOT supported.
- To open a split zip file, use `OpenReader("foo.zip")`. It will open other volumes (.z01 ... .zXX) internally.
- The `zipinsecurepath` env support is removed. It's now always returning ErrInsecurePath error if any file inside the archive uses a non-local name. Additionaly, backslashes in the name are no longer considered as insecure path.

[archive/zip]: https://pkg.go.dev/archive/zip
