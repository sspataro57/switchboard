// board-splitflap (SWT-102, docs/tickets/board-splitflap_SPEC.md D10): the node
// side of TestBoardFlap_*. It is deliberately dumb: it evaluates the board's pure
// flap block and prints what each requested call returned. Every expectation and
// every property check lives in Go (board_flap_test.go), next to golden.json.
//
//   node harness.js <flap block file> <request.json>
//
// The block runs in a fresh vm context with NO host globals: no document, no
// window, no timers, no Date beyond the language built-ins' constructors. A block
// that reaches for the DOM throws here, which is D10's purity rule enforced at
// run time as well as by the Go scan.
//
// request.json is an array of {id, op, ...}. The output is a JSON array of
// {id, value} or {id, error}, in request order. Non-finite numbers are written
// as the strings "Infinity" / "-Infinity" / "NaN", since JSON cannot carry them.
"use strict";

const fs = require("fs");
const vm = require("vm");

const [blockPath, requestPath] = process.argv.slice(2);
const block = fs.readFileSync(blockPath, "utf8");
const requests = JSON.parse(fs.readFileSync(requestPath, "utf8"));

const ctx = vm.createContext({});
vm.runInContext(block, ctx, { filename: "flap-pure-block.js", timeout: 5000 });

function fn(name) {
  const f = ctx[name];
  if (typeof f !== "function") throw new Error("the pure block declares no function " + name);
  return f;
}

function num(v) {
  if (typeof v === "number" && !Number.isFinite(v)) return String(v);
  return v;
}

function unnum(v) {
  if (v === "Infinity") return Infinity;
  if (v === "-Infinity") return -Infinity;
  return v;
}

function run(r) {
  switch (r.op) {
    case "consts": {
      const out = {};
      for (const k of r.names) out[k] = k in ctx ? num(ctx[k]) : null;
      return out;
    }
    case "active":
      return fn("flapActive")(r.pref, r.reduced, r.phone);
    case "reel": {
      const plan = fn("flapAim")(r.shown, r.target);
      const flips = fn("flapFlips")(plan);
      if (typeof flips !== "number" || !Number.isInteger(flips) || flips < 0 || flips > 1000) {
        throw new Error("flapFlips returned " + String(flips));
      }
      const last = Math.max(flips, r.upto || 0);
      const trace = [];
      for (let i = 0; i <= last; i++) trace.push(fn("flapAt")(plan, i));
      return { flips, trace };
    }
    case "drum": {
      const d = fn("drumAim")(r.place, r.shown, r.target, r.full);
      if (!d || typeof d.flips !== "number" || !Number.isInteger(d.flips) || d.flips < 0 || d.flips > 1000) {
        throw new Error("drumAim did not return a drum with an integer .flips: " + JSON.stringify(d));
      }
      const last = Math.max(d.flips, r.upto || 0);
      const trace = [];
      for (let i = 0; i <= last; i++) trace.push(fn("drumAt")(d, i));
      return { flips: d.flips, trace };
    }
    case "shade":
      return fn("flapShade")(r.h, r.full);
    case "fold": {
      const f = fn("flapFold")(r.part);
      return { flap: f && f.flap, scale: f && num(f.scale) };
    }
    case "step":
      return num(fn("flapLineStep")(r.n, r.pageMs));
    case "starts": {
      const s = fn("flapLineStarts")(r.base, r.ends, r.durs, r.gap, unnum(r.step));
      return Array.isArray(s) ? s.map(num) : s;
    }
    default:
      throw new Error("unknown op " + r.op);
  }
}

const out = requests.map((r) => {
  try {
    return { id: r.id, value: run(r) };
  } catch (e) {
    return { id: r.id, error: String(e && e.stack ? e.stack.split("\n").slice(0, 3).join(" | ") : e) };
  }
});
process.stdout.write(JSON.stringify(out));
