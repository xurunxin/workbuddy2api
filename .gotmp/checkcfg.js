const fs = require("fs");
const src = fs.readFileSync("config.example.json", "utf8");
JSON.parse(src);
console.log("valid JSON");
const keys = [];
for (const line of src.split(/\r?\n/)) {
  const m = /^ {2}"([A-Za-z_]+)":/.exec(line);
  if (m) keys.push(m[1]);
}
console.log("top-level keys:", keys.join(", "));
const dup = [...new Set(keys.filter((k, i) => keys.indexOf(k) !== i))];
console.log(dup.length ? "DUPLICATES: " + dup.join(", ") : "no duplicates");
