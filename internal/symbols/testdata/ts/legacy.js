const fs = require("fs");
const { join, resolve: res } = require("path");
const helper = require("./helper").helper;

function load(file, opts = {}, ...rest) {
  return helper(fs.readFileSync(join(file)), opts);
}

class Store extends EventEmitter {
  #items = [];
  count = 0;
  static create() { return new Store(); }
  add(item) { this.#items.push(item); this.emit("add", item); }
  get size() { return this.#items.length; }
  #secret() {}
}

const save = function (store) { return store; };

module.exports = { load, Store };
exports.save = save;
