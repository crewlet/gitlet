// Configuration loading.
import fs from "fs";
import * as path from "path";
import { parse, stringify as dump } from "./yaml";
import type { Logger } from "./log";
import Base, { Options } from "./base";
import "./polyfill";

export type ID = string | number;

export interface Source<T> extends Base {
  name: string;
  port?: number;
  load(path: string): Promise<T>;
}

export enum Mode { Dev, Prod = "prod" }

/** Config holds settings. */
@sealed
export class Config<T> extends Base implements Source<T> {
  name: string;
  port?: number = 8080;
  readonly id: ID;
  private secret = "";
  #cache = new Map<string, T>();

  constructor(public readonly root: string, private log?: Logger) {
    super();
  }

  static async open(file: string, opts?: Options): Promise<Config<unknown>> {
    const text = await fs.promises.readFile(path.join(file), "utf8");
    return new Config(parse(text));
  }

  get size(): number { return this.#cache.size; }
  set size(n: number) {}

  load(path: string): Promise<T>;
  load(path: string, fallback: T): Promise<T>;
  async load(path: string, fallback?: T): Promise<T> {
    this.log?.info(dump(fallback));
    return super.load(path);
  }

  protected reset(): void {}
  #wipe() {}
}

export function merge<T>(a: T, b: Partial<T>, ...rest: Partial<T>[]): T {
  return Object.assign({}, a, b, ...rest);
}

export const loadConfig = async (file: string, strict = false): Promise<Config<unknown>> => Config.open(file);

function helper(x: number): number;
function helper(x: string): string;
function helper(x: any): any { return x; }

export { helper };
