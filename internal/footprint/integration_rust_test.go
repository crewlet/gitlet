package footprint

import "testing"

const rsBase = `use crate::config::{self, load};
use super::util::parse;
use std::collections::HashMap;

pub(crate) fn connect(addr: &str) -> bool {
    load(addr);
    true
}

pub fn send(data: &[u8]) -> usize {
    0
}

pub fn body() -> u8 {
    1
}

fn old_name(a: u8) -> u8 {
    a * 2
}

pub fn narrow() {}

pub struct Client {
    pub name: String,
}

impl Client {
    pub fn new() -> Self {
        Client { name: String::new() }
    }
}
`

const rsHead = `use crate::config::{self, load};
use super::util::parse;
use std::collections::HashMap;

pub fn connect(addr: &str) -> bool {
    load(addr);
    true
}

pub fn send(data: &[u8], flags: u32) -> usize {
    config::read(data);
    parse(data);
    0
}

pub fn body() -> u8 {
    let m: HashMap<u8, u8> = HashMap::new();
    crate::net::dial();
    super::helpers::go();
    inner::f();
    m.len() as u8
}

fn new_name(a: u8) -> u8 {
    a * 2
}

pub(crate) fn narrow() {}

pub struct Client {
    pub name: String,
}

impl Client {
    pub fn new() -> Self {
        let c = Self::build();
        c.send();
        Client { name: String::new() }
    }
}

mod inner {
    pub fn f() {}
}
`

func TestIntegrationRust(t *testing.T) {
	res := computeReal(t, "",
		change{status: "M", path: "src/net/client.rs", base: rsBase, head: rsHead},
		change{status: "R", path: "src/old/mod.rs", oldPath: "src/old.rs",
			base: "pub fn f() {}\n", head: "pub fn f() {}\n"},
	)
	checkLines(t, "symbols", summarize(res.Footprint.Symbols), []string{
		"src/net/client.rs:Client.new body",
		"src/net/client.rs:body body",
		"src/net/client.rs:connect signature", // pub(crate) to pub
		"src/net/client.rs:inner.f added",
		"src/net/client.rs:narrow signature!", // pub to pub(crate)
		"src/net/client.rs:new_name renamed! from old_name",
		"src/net/client.rs:send signature!", // a required parameter appended
		// old.rs to old/mod.rs keeps module old.
		"src/old/mod.rs:f moved from src/old.rs sig=false body=false",
	})
	// HashMap and String::new are prelude or std and dropped.
	checkLines(t, "refs", summarizeRefs(res.Footprint.Refs), []string{
		"src/net/client.rs:6 load src/config",                    // use list {self, load}
		"src/net/client.rs:11 config.read src/config",            // use list {self}
		"src/net/client.rs:12 parse src/net/util",                // use super::util::parse
		"src/net/client.rs:18 crate::net.dial src/net",           // crate:: path
		"src/net/client.rs:19 super::helpers.go src/net/helpers", // super:: path
		"src/net/client.rs:20 inner.f src/net/client",            // inline module
		"src/net/client.rs:21 m.len ?",
		"src/net/client.rs:36 Client.build src/net/client", // Self rewritten to the impl type
		"src/net/client.rs:37 c.send ?",                    // a receiver, not a module
		"src/net/client.rs:38 Client src/net/client",
	})
}
