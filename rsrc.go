package main

// TODO:
// - to store icons, see: http://blogs.msdn.com/b/oldnewthing/archive/2012/07/20/10331787.aspx
//   - also need to first read and split input ico file

import (
	"debug/pe"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"os"
	"path"
	"reflect"

	"github.com/akavel/rsrc/ico"
)

type ImageResourceDirectory struct {
	Characteristics      uint32
	TimeDateStamp        uint32
	MajorVersion         uint16
	MinorVersion         uint16
	NumberOfNamedEntries uint16
	NumberOfIdEntries    uint16
}

type ImageResourceDirectoryEntry struct {
	NameOrId     uint32
	OffsetToData uint32
}

type ImageResourceDataEntry struct {
	OffsetToData uint32
	Size1        uint32
	CodePage     uint32
	Reserved     uint32
}

type RelocationEntry struct {
	RVA         uint32 // "offset within the Section's raw data where the address starts."
	SymbolIndex uint32 // "(zero based) index in the Symbol table to which the reference refers."
	Type        uint16
}

type Symbol struct {
	Name           [8]byte
	Value          uint32
	SectionNumber  uint16
	Type           uint16
	StorageClass   uint8
	AuxiliaryCount uint8
}

type StringsHeader struct {
	Length uint32
}

const (
	MASK_SUBDIRECTORY = 1 << 31

	RT_ICON       = 3
	RT_GROUP_ICON = 3 + 11
	RT_MANIFEST   = 24
)

var (
	STRING_RSRC = [8]byte{'.', 'r', 's', 'r', 'c', 0, 0, 0}
)

func MustGetFieldOffset(t reflect.Type, field string) uintptr {
	f, ok := t.FieldByName(field)
	if !ok {
		panic("field " + field + " not found")
	}
	return f.Offset
}

func main() {
	//TODO: allow in options advanced specification of multiple resources, as a tree (json?)
	var fnamein, fnameico, fnameout string
	flags := flag.NewFlagSet("", flag.ContinueOnError)
	flags.StringVar(&fnamein, "manifest", "", "path to a Windows manifest file to embed")
	flags.StringVar(&fnameico, "ico", "", "UNSUPPORTED: path to ICO file to embed")
	flags.StringVar(&fnameout, "o", "rsrc.syso", "name of output COFF (.res or .syso) file")
	_ = flags.Parse(os.Args[1:])
	if fnamein == "" {
		fmt.Fprintf(os.Stderr, "USAGE: %s -manifest FILE.exe.manifest [-o FILE.syso]\n"+
			"Generates a .syso file with specified resources embedded in .rsrc section,\n"+
			"aimed for consumption by Go linker when building Win32 excecutables.\n"+
			"OPTIONS:\n",
			os.Args[0])
		flags.PrintDefaults()
		os.Exit(1)
	}

	err := run(fnamein, fnameico, fnameout)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type Coff struct {
	pe.FileHeader
	pe.SectionHeader32

	Data []interface{}

	Relocations []RelocationEntry
	Symbols     []Symbol
	StringsHeader
}

func run(fnamein, fnameico, fnameout string) error {
	manifest, err := SizedOpen(fnamein)
	if err != nil {
		return fmt.Errorf("Error opening manifest file '%s': %s", fnamein, err)
	}
	defer manifest.Close()

	if fnameico != "" {
		tmpf, err := os.Open(fnameico)
		if err != nil {
			return err
		}
		defer tmpf.Close()
		_, err = ico.DecodeHeaders(tmpf)
		if err != nil {
			return err
		}
	}

	out, err := os.Create(fnameout)
	if err != nil {
		return err
	}
	defer out.Close()
	w := Writer{W: out}

	coff := Coff{
		pe.FileHeader{
			Machine:              0x014c, //FIXME: find out how to differentiate this value, or maybe not necessary for Go
			NumberOfSections:     1,      // .rsrc
			TimeDateStamp:        0,      // was also 0 in sample data from MinGW's windres.exe
			NumberOfSymbols:      1,
			SizeOfOptionalHeader: 0,
			Characteristics:      0x0104, //FIXME: copied from windres.exe output, find out what should be here and why
		},
		pe.SectionHeader32{
			Name:                STRING_RSRC,
			NumberOfRelocations: 1,
			Characteristics:     0x40000040, // "INITIALIZED_DATA MEM_READ" ?
		},

		// now, build "directory hierarchy" of .rsrc section: first type, then id/name, then language
		[]interface{}{
			&ImageResourceDirectory{
				NumberOfIdEntries: 1,
			},
			&ImageResourceDirectoryEntry{
				NameOrId: RT_MANIFEST,
			},
			&ImageResourceDirectory{
				NumberOfIdEntries: 1,
			},
			&ImageResourceDirectoryEntry{
				NameOrId: 1, // ID
			},
			&ImageResourceDirectory{
				NumberOfIdEntries: 1,
			},
			&ImageResourceDirectoryEntry{
				NameOrId: 0x0409, //FIXME: language; what value should be here?
			},

			&ImageResourceDataEntry{
				Size1:    uint32(manifest.Size()),
				CodePage: 0, //FIXME: what value here? for now just tried 0
			},

			manifest,
		},

		[]RelocationEntry{RelocationEntry{
			SymbolIndex: 0, // "(zero based) index in the Symbol table to which the reference refers. Once you have loaded the COFF file into memory and know where each symbol is, you find the new updated address for the given symbol and update the reference accordingly."
			Type:        7, // according to ldpe.c, this decodes to: IMAGE_REL_I386_DIR32NB
		}},

		[]Symbol{Symbol{
			Name:           STRING_RSRC,
			Value:          0,
			SectionNumber:  1,
			Type:           0, // FIXME: wtf?
			StorageClass:   3, // FIXME: is it ok? and uint8? and what does the value mean?
			AuxiliaryCount: 0, // FIXME: wtf?
		}},

		StringsHeader{
			Length: uint32(binary.Size(StringsHeader{})), // empty strings table -- but we must still show size of the table's header...
		},
	}

	// fill in some important offsets in resulting file
	var offset, diroff uint32
	Walk(coff, func(v reflect.Value, path string) error {
		switch path {
		case "/Data":
			coff.SectionHeader32.PointerToRawData = offset
			diroff = offset
		case "/Data[2]":
			coff.Data[1].(*ImageResourceDirectoryEntry).OffsetToData = MASK_SUBDIRECTORY | (offset - diroff)
		case "/Data[4]":
			coff.Data[3].(*ImageResourceDirectoryEntry).OffsetToData = MASK_SUBDIRECTORY | (offset - diroff)
		case "/Data[6]":
			coff.Data[5].(*ImageResourceDirectoryEntry).OffsetToData = offset - diroff
		case "/Data[6]/OffsetToData":
			coff.Relocations[0].RVA = offset - diroff
		case "/Data[7]":
			coff.Data[6].(*ImageResourceDataEntry).OffsetToData = offset - diroff
		case "/Relocations":
			coff.SectionHeader32.PointerToRelocations = offset
			coff.SectionHeader32.SizeOfRawData = offset - diroff
		case "/Symbols":
			coff.FileHeader.PointerToSymbolTable = offset
		}

		if Plain(v.Kind()) {
			offset += uint32(binary.Size(v.Interface())) // TODO: change to v.Type().Size() ?
			return nil
		}
		vv, ok := v.Interface().(SizedReader)
		if ok {
			offset += uint32(vv.Size())
			return WALK_SKIP
		}
		return nil
	})

	// write the resulting file to disk
	Walk(coff, func(v reflect.Value, path string) error {
		if Plain(v.Kind()) {
			w.WriteLE(v.Interface())
			return nil
		}
		vv, ok := v.Interface().(SizedReader)
		if ok {
			w.WriteFromSized(vv)
			return WALK_SKIP
		}
		return nil
	})

	if w.Err != nil {
		return fmt.Errorf("Error writing output file: %s", w.Err)
	}

	return nil
}

var (
	WALK_SKIP = errors.New("")
)

type Walker func(v reflect.Value, path string) error

func Walk(value interface{}, walker Walker) error {
	err := walk(reflect.ValueOf(value), "/", walker)
	if err == WALK_SKIP {
		err = nil
	}
	return err
}

func stopping(err error) bool {
	return err != nil && err != WALK_SKIP
}

func walk(v reflect.Value, spath string, walker Walker) error {
	err := walker(v, spath)
	if err != nil {
		return err
	}
	v = reflect.Indirect(v)
	switch v.Type().Kind() {
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			err = walk(v.Index(i), spath+fmt.Sprintf("[%d]", i), walker)
			if stopping(err) {
				return err
			}
		}
	case reflect.Interface:
		err = walk(v.Elem(), spath, walker)
		if stopping(err) {
			return err
		}
	case reflect.Struct:
		//t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			//f := t.Field(i) //TODO: handle unexported fields
			vv := v.Field(i)
			err = walk(vv, path.Join(spath, v.Type().Field(i).Name), walker)
			if stopping(err) {
				return err
			}
		}
	default:
		// FIXME: handle other special cases too
		// String
		return nil
	}
	return nil
}

func Plain(kind reflect.Kind) bool {
	switch kind {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr, reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128:
		return true
	}
	return false
}
