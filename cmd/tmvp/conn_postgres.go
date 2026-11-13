package main

// Each connector is linked by its own conn_<type>.go file so parallel agents
// never edit the same file.
import _ "github.com/muthuishere/tmvp/connectors/postgres"
