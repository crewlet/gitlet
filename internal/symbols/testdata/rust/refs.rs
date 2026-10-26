use crate::model::{Config, Error};

pub struct Wrapper<T: Store> {
    pub inner: T,
    pub cfg: crate::model::Config,
}

impl<T: Store> Wrapper<T> {
    pub fn open(path: &Path) -> Result<Self, Error> {
        let raw = fs::read(path)?;
        let cfg = model::parse::<Config>(&raw)?;
        let w = Self::with(cfg);
        let s = Self { inner: T::default(), cfg: w.cfg.clone() };
        assert_eq!(s.cfg.name(), helper(1));
        Ok(s)
    }
}

impl Drop for Wrapper<Mem> {
    fn drop(&mut self) {
        self.inner.flush().unwrap();
        crate::log::Entry { level: 1 };
    }
}

pub trait Store: Default + io::Read {
    fn get(&self) -> Option<Self::Out>;
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn opens() {
        Wrapper::<Mem>::open(Path::new("x")).unwrap();
    }
}
