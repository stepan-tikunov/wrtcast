// sectionlab measures how close Claude marks the recap, the intro and the
// credits to the provider's sections, on episodes the provider has marked.
//
//	sectionlab fetch -dir DIR -title ID s2e5 s3e2 ...   loads the fixtures once
//	sectionlab run -dir DIR [s2e5 ...]                  marks them and compares
//
// run reads the API key from ANTHROPIC_API_KEY and touches nothing but the
// Claude API, so prompts can be tried again and again on the same fixtures.
package main

import (
	"fmt"
	"log"
	"os"
)

func main() {
	log.SetFlags(log.Ltime)
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: sectionlab fetch|run ...")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "fetch":
		err = runFetch(os.Args[2:])
	case "run":
		err = runMark(os.Args[2:])
	case "frames":
		err = runFrames(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		log.Fatal(err)
	}
}
