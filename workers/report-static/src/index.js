import routes from './routes.json';
import { createHandler } from './handler.mjs';
export default createHandler(JSON.parse(routes));
