This is a fork of the [yeka/zip](https://github.com/yeka/zip) to add support for reading split zip files (.z01 + ... + .zXX + .zip).

Notes

- Only reading of split zip files is implemented. Writing is NOT supported.
- To open a split zip file, use `OpenReader("foo.zip")`. It will open other volumes (.z01 ... .zXX) internally.
- If any file inside the archive uses a non-local name, it returns opened zip file along with `ErrInsecurePath` error. It's the same behavior as Go standard [archive/zip](https://pkg.go.dev/archive/zip) package. However, backslashes in the name are no longer considered as insecure path.

# yeka/zip

This fork add support for Standard Zip Encryption.

The work is based on https://github.com/alexmullins/zip

Available encryption:

```
zip.StandardEncryption
zip.AES128Encryption
zip.AES192Encryption
zip.AES256Encryption
```

## Warning

Zip Standard Encryption isn't actually secure.
Unless you have to work with it, please use AES encryption instead.

## Example Encrypt Zip

```
package main

import (
	"bytes"
	"io"
	"log"
	"os"

	"github.com/yeka/zip"
)

func main() {
	contents := []byte("Hello World")
	fzip, err := os.Create(`./test.zip`)
	if err != nil {
		log.Fatalln(err)
	}
	zipw := zip.NewWriter(fzip)
	defer zipw.Close()
	w, err := zipw.Encrypt(`test.txt`, `golang`, zip.AES256Encryption)
	if err != nil {
		log.Fatal(err)
	}
	_, err = io.Copy(w, bytes.NewReader(contents))
	if err != nil {
		log.Fatal(err)
	}
	zipw.Flush()
}
```

## Example Decrypt Zip

```
package main

import (
	"fmt"
	"io/ioutil"
	"log"

	"github.com/yeka/zip"
)

func main() {
	r, err := zip.OpenReader("encrypted.zip")
	if err != nil {
		log.Fatal(err)
	}
	defer r.Close()

	for _, f := range r.File {
		if f.IsEncrypted() {
			f.SetPassword("12345")
		}

		r, err := f.Open()
		if err != nil {
			log.Fatal(err)
		}

		buf, err := ioutil.ReadAll(r)
		if err != nil {
			log.Fatal(err)
		}
		defer r.Close()

		fmt.Printf("Size of %v: %v byte(s)\n", f.Name, len(buf))
	}
}
```
