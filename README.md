# gointerfacegen

A Go tool used to generate an interface from a type's methods.

## Installation

```bash
go get github.com/hankjacobs/gointerfacegen
```

## Usage

```text
gointefacegen <type> <interface> <file>

Generates an interface from the type's methods found in the specified file. File must be valid go source.
If the already interface exists, it is updated in place with the methods found for the type.
Default behavior prints the resulting file containing the interface to standard out.

Examples:
gointefacegen somecustomtype somecustominterface src.go

  -i    Print only interface to standard out. This takes precedence over -w flag
  -w    Write result to file instead of stdout
```

## Example

Given a file `demo.go`:

```go
package demo

type example struct {
}

func (t example) First() {
}

func (t example) Second(one, two string) (named example, other example) {
    return
}

```

running:

```shell
gointerfacegen example ExampleInteface demo.go
```

will produce:

```go
package demo

type ExampleInterface interface {
    First()
    Second(one, two string) (example, example)
}

type example struct {
}

func (t example) First() {
}

func (t example) Second(one, two string) (named example, other example) {
    return
}
```