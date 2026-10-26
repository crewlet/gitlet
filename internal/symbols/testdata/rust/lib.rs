//! Configuration loading.

use std::collections::HashMap;
use std::fmt::{self, Display};
use std::io::{self as stdio, Read as _};
use crate::store::{Store, kv::{Get, Put as Insert}};
use super::*;
use serde;
pub use crate::error::Error;

/// The highest port number.
pub const MAX_PORT: u16 = 65535;
pub(crate) static mut COUNT: u32 = 0;
static NAME: &str = "gitlet";
const _: () = ();

pub type Result<T> = std::result::Result<T, Error>;

#[derive(Debug, Clone)]
pub struct Config<T> {
    pub name: String,
    pub(crate) port: u16,
    secret: T,
}

pub struct Id(pub u64, String);

pub struct Marker;

pub enum Shape<T> {
    Circle(f64),
    Rect { w: f64, h: f64 },
    Point(T),
    Empty,
}

pub trait Source: Send + Sync {
    type Item: Clone;
    const LIMIT: usize;
    fn get(&self, key: &str) -> Option<Vec<u8>>;
    fn len(&self) -> usize {
        0
    }
}

pub fn load(path: &str, strict: bool) -> Result<Config<String>> {
    let text = std::fs::read_to_string(path)?;
    let cfg: Config<String> = parse(&text, strict)?;
    Ok(cfg)
}

pub async fn fetch<T, F: Fn(T) -> u8>(items: Vec<T>, f: F) -> Vec<u8>
where
    T: Clone,
{
    items.into_iter().map(f).collect()
}

fn parse(text: &str, _strict: bool) -> Result<Config<String>> {
    let port: u16 = MAX_PORT;
    println!("{}", helper::trim(text));
    Ok(Config { name: text.to_string(), port, secret: String::new() })
}

impl<T: Clone> Config<T> {
    pub fn new(name: &str, secret: T) -> Self {
        Self { name: name.into(), port: 0, secret }
    }

    pub fn port(&self) -> u16 {
        self.port
    }

    pub(crate) fn set_port(&mut self, port: u16) {
        self.port = port;
        self.validate();
    }

    fn validate(&self) {}

    pub const DEFAULT_PORT: u16 = 8080;
}

impl<T> fmt::Display for Config<T> {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}", self.name)
    }
}

impl Iterator for Id {
    type Item = u64;

    fn next(&mut self) -> Option<u64> {
        None
    }
}

pub mod helper {
    pub fn trim(s: &str) -> &str {
        s.trim()
    }

    pub struct Buf;

    impl Buf {
        pub fn clear(self) {}
    }

    mod inner {
        pub(super) fn deep() -> super::Buf {
            super::Buf
        }
    }
}

macro_rules! log {
    ($x:expr) => {
        println!("{}", $x)
    };
}

extern "C" {
    fn abs(x: i32) -> i32;
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn loads() {
        let cfg = load("x", true).unwrap();
        assert_eq!(cfg.port(), 0);
    }
}
