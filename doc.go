/*
    This package provides basic Server support for Vibe: http://vibe-project.github.io/


    Creating a Server:

        // create the server, specifying which protocols you want to support, and your heartbeat timeout
        v := vibe.NewServer(nil, 0)
        // assign your listener
        // create your own listener. It must implement the ServerListener interface
        var sl myServerListener
        v.Listener = sl

        // attach to the standard http handler
        http.Handle("/vibe", v)
        // start listening
        http.ListenAndServe(":8000", nil)

*/
package vibe
