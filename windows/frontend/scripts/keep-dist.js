// vite build empties dist/, which would remove the committed .gitkeep that
// go:embed all:frontend/dist needs on a fresh checkout. Recreate it after
// every build.
import { writeFileSync } from "fs";

writeFileSync("dist/.gitkeep", "");
