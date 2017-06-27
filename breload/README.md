# Golang page reloader embedded in gopherjs tool 

This is a live browser reloader package for gopherjs toot which can programmatically refresh a tab in a browser.
Author initial code and idea is Mark Vincze https://github.com/markvincze.
He write post about it on own blog https://blog.markvincze.com/programmatically-refreshing-a-browser-tab-from-a-golang-application/.
I only connect libs (https://github.com/fsnotify/fsnotify and https://github.com/markvincze/golang-reload-browser/blob/master/wsHub.go) and make some changes to embed in gopherjs.

## Usage

Copy bone file index.html on yours gopherjs working directory and make some needs changes.


Then start up the gopherjs as server.

```
gopherjs serve 
```
(This will watch current directory(where is running) and when files are changed it will reload browser).

Then open the file `index.html` in the browser and happy work on your gopherjs project:-).

