package breload

/*      https://github.com/fsnotify/fsnotify/blob/master/example_test.go
        https://github.com/markvincze/golang-reload-browser/blob/master/main.go
        Author and Idea:Mark Vincze https://github.com/markvincze
        Author(2017):Evgeniy Kudinov https://github.com/ekudinov
*/

import (
	"log"

	"github.com/fsnotify/fsnotify"
)

// StartBrowserReloader start file watcher and
// reloads browser when files are changed
func StartBrowserReloader() {
	log.Println("Starting reload server.")

	startReloadServer()

	log.Println("Reload server started.")

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer watcher.Close()

	done := make(chan bool)
	go func() {
		for {
			select {
			case event := <-watcher.Events:
				if event.Op&fsnotify.Write == fsnotify.Write {
					log.Println("modified file:", event.Name)
					log.Println("reload browser")
					sendReload()
				}
			case err := <-watcher.Errors:
				log.Println("error:", err)
			}
		}
	}()

	err = watcher.Add(".")
	if err != nil {
		log.Fatal(err)
	}
	<-done
}
